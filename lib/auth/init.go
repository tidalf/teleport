/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"
)

// InitConfig is auth server init config
type InitConfig struct {
	// Backend is auth backend to use
	Backend backend.Backend

	// Authority is key generator that we use
	Authority Authority

	// HostUUID is a UUID of this host
	HostUUID string

	// NodeName is the DNS name of the node
	NodeName string

	// DomainName stores the FQDN of the signing CA (its certificate will have this
	// name embedded). It is usually set to the GUID of the host the Auth service runs on
	DomainName string

	// Authorities is a list of pre-configured authorities to supply on first start
	Authorities []services.CertAuthority

	// AuthServiceName is a human-readable name of this CA. If several Auth services are running
	// (managing multiple teleport clusters) this field is used to tell them apart in UIs
	// It usually defaults to the hostname of the machine the Auth service runs on.
	AuthServiceName string

	// DataDir is the full path to the directory where keys, events and logs are kept
	DataDir string

	// ReverseTunnels is a list of reverse tunnels statically supplied
	// in configuration, so auth server will init the tunnels on the first start
	ReverseTunnels []services.ReverseTunnel

	// OIDCConnectors is a list of trusted OpenID Connect identity providers
	// in configuration, so auth server will init the tunnels on the first start
	OIDCConnectors []services.OIDCConnector

	// Trust is a service that manages users and credentials
	Trust services.Trust

	// Presence service is a discovery and hearbeat tracker
	Presence services.Presence

	// Provisioner is a service that keeps track of provisioning tokens
	Provisioner services.Provisioner

	// Identity is a service that manages users and credentials
	Identity services.Identity

	// Access is service controlling access to resources
	Access services.Access

	// ClusterAuthPreferenceService is a service to get and set authentication preferences.
	ClusterAuthPreferenceService services.ClusterAuthPreference

	// UniversalSecondFactorService is a service to get and set universal second factor settings.
	UniversalSecondFactorService services.UniversalSecondFactorSettings

	// Roles is a set of roles to create
	Roles []services.Role

	// StaticTokens are pre-defined host provisioning tokens supplied via config file for
	// environments where paranoid security is not needed
	StaticTokens []services.ProvisionToken

	// AuthPreference defines the authentication type (local, oidc) and second
	// factor (off, otp, u2f) passed in from a configuration file.
	AuthPreference services.AuthPreference

	// U2F defines U2F application ID and any facets passed in from a configuration file.
	U2F services.UniversalSecondFactor
}

// Init instantiates and configures an instance of AuthServer
func Init(cfg InitConfig, seedConfig bool) (*AuthServer, *Identity, error) {
	if cfg.DataDir == "" {
		return nil, nil, trace.BadParameter("DataDir: data dir can not be empty")
	}
	if cfg.HostUUID == "" {
		return nil, nil, trace.BadParameter("HostUUID: host UUID can not be empty")
	}

	err := cfg.Backend.AcquireLock(cfg.DomainName, 30*time.Second)
	if err != nil {
		return nil, nil, err
	}
	defer cfg.Backend.ReleaseLock(cfg.DomainName)

	// check that user CA and host CA are present and set the certs if needed
	asrv := NewAuthServer(&cfg)

	// we determine if it's the first start by checking if the CA's are set
	firstStart, err := isFirstStart(asrv, cfg)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	// we skip certain configuration if 'seed_config' is set to true
	// and this is NOT the first time teleport starts on this machine
	skipConfig := seedConfig && !firstStart

	// upon first start, set the cluster auth prerference from the configuration file
	// and create a resource on the backend, after that always read from the backend
	if firstStart {
		log.Infof("Initializing Cluster Authentication Preference: %v", cfg.AuthPreference)
		err = asrv.SetClusterAuthPreference(cfg.AuthPreference)
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}

		if cfg.U2F != nil {
			log.Infof("Initializing Universal Second Factor Settings: %v", cfg.U2F)
			err = asrv.SetUniversalSecondFactor(cfg.U2F)
			if err != nil {
				return nil, nil, trace.Wrap(err)
			}
		}
	}

	// add trusted authorities from the configuration into the trust backend:
	keepMap := make(map[string]int, 0)
	if !skipConfig {
		log.Infof("Initializing roles")
		for _, role := range cfg.Roles {
			if err := asrv.UpsertRole(role); err != nil {
				return nil, nil, trace.Wrap(err)
			}
		}

		log.Infof("Initializing cert authorities")
		for i := range cfg.Authorities {
			ca := cfg.Authorities[i]
			ca, err = services.GetCertAuthorityMarshaler().GenerateCertAuthority(ca)
			if err != nil {
				return nil, nil, trace.Wrap(err)
			}
			if err := asrv.Trust.UpsertCertAuthority(ca, backend.Forever); err != nil {
				return nil, nil, trace.Wrap(err)
			}
			keepMap[ca.GetClusterName()] = 1
		}
	}
	// delete trusted authorities from the trust back-end if they're not
	// in the configuration:
	if !seedConfig {
		hostCAs, err := asrv.Trust.GetCertAuthorities(services.HostCA, false)
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}
		userCAs, err := asrv.Trust.GetCertAuthorities(services.UserCA, false)
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}
		for _, ca := range append(hostCAs, userCAs...) {
			_, configured := keepMap[ca.GetClusterName()]
			if ca.GetClusterName() != cfg.DomainName && !configured {
				if err = asrv.Trust.DeleteCertAuthority(ca.GetID()); err != nil {
					return nil, nil, trace.Wrap(err)
				}
				log.Infof("removed old trusted CA: '%s'", ca.GetClusterName())
			}
		}
	}
	// this block will generate user CA authority on first start if it's
	// not currently present, it will also use optional passed user ca keypair
	// that can be supplied in configuration
	if _, err := asrv.GetCertAuthority(services.CertAuthID{DomainName: cfg.DomainName, Type: services.HostCA}, false); err != nil {
		if !trace.IsNotFound(err) {
			return nil, nil, trace.Wrap(err)
		}
		log.Infof("FIRST START: Generating host CA on first start")
		priv, pub, err := asrv.GenerateKeyPair("")
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}
		hostCA := &services.CertAuthorityV2{
			Kind:    services.KindCertAuthority,
			Version: services.V2,
			Metadata: services.Metadata{
				Name:      cfg.DomainName,
				Namespace: defaults.Namespace,
			},
			Spec: services.CertAuthoritySpecV2{
				ClusterName:  cfg.DomainName,
				Type:         services.HostCA,
				SigningKeys:  [][]byte{priv},
				CheckingKeys: [][]byte{pub},
			},
		}
		if err := asrv.Trust.UpsertCertAuthority(hostCA, backend.Forever); err != nil {
			return nil, nil, trace.Wrap(err)
		}
	}
	// this block will generate user CA authority on first start if it's
	// not currently present, it will also use optional passed user ca keypair
	// that can be supplied in configuration
	if _, err := asrv.GetCertAuthority(services.CertAuthID{DomainName: cfg.DomainName, Type: services.UserCA}, false); err != nil {
		if !trace.IsNotFound(err) {
			return nil, nil, trace.Wrap(err)
		}

		log.Infof("FIRST START: Generating user CA on first start")
		priv, pub, err := asrv.GenerateKeyPair("")
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}
		userCA := &services.CertAuthorityV2{
			Kind:    services.KindCertAuthority,
			Version: services.V2,
			Metadata: services.Metadata{
				Name:      cfg.DomainName,
				Namespace: defaults.Namespace,
			},
			Spec: services.CertAuthoritySpecV2{
				ClusterName:  cfg.DomainName,
				Type:         services.UserCA,
				SigningKeys:  [][]byte{priv},
				CheckingKeys: [][]byte{pub},
			},
		}
		if err := asrv.Trust.UpsertCertAuthority(userCA, backend.Forever); err != nil {
			return nil, nil, trace.Wrap(err)
		}
	}
	// add reverse runnels from the configuration into the backend
	keepMap = make(map[string]int, 0)
	if !skipConfig {
		log.Infof("Initializing reverse tunnels")
		for _, tunnel := range cfg.ReverseTunnels {
			if err := asrv.UpsertReverseTunnel(tunnel, 0); err != nil {
				return nil, nil, trace.Wrap(err)
			}
			keepMap[tunnel.GetClusterName()] = 1
		}
	}

	// remove the reverse tunnels from the backend if they're not
	// present in the configuration
	if !seedConfig {
		tunnels, err := asrv.GetReverseTunnels()
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}
		for _, tunnel := range tunnels {
			_, configured := keepMap[tunnel.GetClusterName()]
			if !configured {
				if err = asrv.DeleteReverseTunnel(tunnel.GetClusterName()); err != nil {
					return nil, nil, trace.Wrap(err)
				}
				log.Infof("removed reverse tunnel: '%s'", tunnel.GetClusterName())
			}
		}
	}

	// add OIDC connectors to the back-end. we always add connectors to the backend
	// because settings can come from legacy configuration so we keep doing this
	// until we remove support for legacy formats.
	keepMap = make(map[string]int, 0)
	if !skipConfig {
		log.Infof("Initializing OIDC connectors")
		for _, connector := range cfg.OIDCConnectors {
			if err := asrv.UpsertOIDCConnector(connector, 0); err != nil {
				return nil, nil, trace.Wrap(err)
			}
			log.Infof("created ODIC connector '%s'", connector.GetName())
			keepMap[connector.GetName()] = 1
		}
	}
	// remove OIDC connectors from the backend if they're not
	// present in the configuration
	if !seedConfig {
		connectors, _ := asrv.GetOIDCConnectors(false)
		for _, connector := range connectors {
			_, configured := keepMap[connector.GetName()]
			if !configured {
				if err = asrv.DeleteOIDCConnector(connector.GetName()); err != nil {
					return nil, nil, trace.Wrap(err)
				}
				log.Infof("removed OIDC connector '%s'", connector.GetName())
			}
		}
	}

	// migrate u2f settings to the backend. we do this because settings can come from legacy
	// configuration so always push to the backend until we remove support for the legacy format.
	if cfg.U2F != nil {
		err = asrv.SetUniversalSecondFactor(cfg.U2F)
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}
	}

	// create default namespace
	err = asrv.UpsertNamespace(services.NewNamespace(defaults.Namespace))
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	// migrate old users to new format
	users, err := asrv.GetUsers()
	for i := range users {
		user := users[i]
		raw, ok := (user.GetRawObject()).(services.UserV1)
		if !ok {
			continue
		}
		log.Infof("migrating legacy user %v", user.GetName())
		role := services.RoleForUser(user)
		role.SetLogins(raw.AllowedLogins)
		err = asrv.UpsertRole(role)
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}
		user.AddRole(role.GetName())
		if err := asrv.UpsertUser(user); err != nil {
			return nil, nil, trace.Wrap(err)
		}
	}

	// migrate old cert authorities
	cas, err := asrv.GetCertAuthorities(services.UserCA, true)
	for i := range cas {
		ca := cas[i]
		if err := migrateCertAuthority(asrv, ca); err != nil {
			return nil, nil, trace.Wrap(err)
		}
	}

	identity, err := initKeys(asrv, cfg.DataDir,
		IdentityID{HostUUID: cfg.HostUUID, NodeName: cfg.NodeName, Role: teleport.RoleAdmin})
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return asrv, identity, nil
}

func migrateCertAuthority(asrv *AuthServer, in services.CertAuthority) error {
	raw, ok := (in.GetRawObject()).(services.CertAuthorityV1)
	if !ok {
		return nil
	}
	_, err := asrv.GetRole(services.RoleNameForCertAuthority(in.GetClusterName()))
	if err == nil {
		return nil
	}
	if !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	ca, role := services.ConvertV1CertAuthority(&raw)
	log.Infof("migrating legacy cert authority %v", in.GetName())
	err = asrv.UpsertRole(role)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := asrv.UpsertCertAuthority(ca, 0); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// isFirstStart returns 'true' if the auth server is starting for the 1st time
// on this server.
func isFirstStart(authServer *AuthServer, cfg InitConfig) (bool, error) {
	// check if the CA exists?
	_, err := authServer.GetCertAuthority(
		services.CertAuthID{
			DomainName: cfg.DomainName,
			Type:       services.HostCA,
		}, false)
	if err != nil {
		if !trace.IsNotFound(err) {
			return false, trace.Wrap(err)
		}
		// CA not found? --> first start!
		return true, nil
	}
	return false, nil
}

// initKeys initializes a nodes host certificate. If the certificate does not exist, a request
// is made to the certificate authority to generate a host certificate and it's written to disk.
// If a certificate exists on disk, it is read in and returned.
func initKeys(a *AuthServer, dataDir string, id IdentityID) (*Identity, error) {
	kp, cp := keysPath(dataDir, id)

	keyExists, err := pathExists(kp)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	certExists, err := pathExists(cp)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if !keyExists || !certExists {
		packedKeys, err := a.GenerateServerKeys(id.HostUUID, id.NodeName, teleport.Roles{id.Role})
		if err != nil {
			return nil, trace.Wrap(err)
		}

		err = writeKeys(dataDir, id, packedKeys.Key, packedKeys.Cert)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	i, err := ReadIdentity(dataDir, id)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return i, nil
}

// writeKeys saves the key/cert pair for a given domain onto disk. This usually means the
// domain trusts us (signed our public key)
func writeKeys(dataDir string, id IdentityID, key []byte, cert []byte) error {
	kp, cp := keysPath(dataDir, id)
	log.Debugf("write key to %v, cert from %v", kp, cp)

	if err := ioutil.WriteFile(kp, key, 0600); err != nil {
		return err
	}
	if err := ioutil.WriteFile(cp, cert, 0600); err != nil {
		return err
	}
	return nil
}

// Identity is a collection of certificates and signers that represent identity
type Identity struct {
	ID              IdentityID
	KeyBytes        []byte
	CertBytes       []byte
	KeySigner       ssh.Signer
	Cert            *ssh.Certificate
	AuthorityDomain string
}

// IdentityID is a combination of role, host UUID, and node name.
type IdentityID struct {
	Role     teleport.Role
	HostUUID string
	NodeName string
}

// Equals returns true if two identities are equal
func (id *IdentityID) Equals(other IdentityID) bool {
	return id.Role == other.Role && id.HostUUID == other.HostUUID
}

// String returns debug friendly representation of this identity
func (id *IdentityID) String() string {
	return fmt.Sprintf("Identity(hostuuid=%v, role=%v)", id.HostUUID, id.Role)
}

// ReadIdentityFromKeyPair reads identity from initialized keypair
func ReadIdentityFromKeyPair(keyBytes, certBytes []byte) (*Identity, error) {
	if len(keyBytes) == 0 {
		return nil, trace.BadParameter("PrivateKey: missing private key")
	}

	if len(certBytes) == 0 {
		return nil, trace.BadParameter("Cert: missing parameter")
	}

	pubKey, _, _, _, err := ssh.ParseAuthorizedKey(certBytes)
	if err != nil {
		return nil, trace.BadParameter("failed to parse server certificate: %v", err)
	}

	cert, ok := pubKey.(*ssh.Certificate)
	if !ok {
		return nil, trace.BadParameter("expected ssh.Certificate, got %v", pubKey)
	}

	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, trace.BadParameter("failed to parse private key: %v", err)
	}
	// this signer authenticates using certificate signed by the cert authority
	// not only by the public key
	certSigner, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		return nil, trace.BadParameter("unsupported private key: %v", err)
	}

	// check principals on certificate
	if len(cert.ValidPrincipals) < 1 {
		return nil, trace.BadParameter("valid principals: at least one valid principal is required")
	}
	for _, validPrincipal := range cert.ValidPrincipals {
		if validPrincipal == "" {
			return nil, trace.BadParameter("valid principal can not be empty: %q", cert.ValidPrincipals)
		}
	}

	// check permissions on certificate
	if len(cert.Permissions.Extensions) == 0 {
		return nil, trace.BadParameter("extensions: misssing needed extensions for host roles")
	}
	roleString := cert.Permissions.Extensions[utils.CertExtensionRole]
	if roleString == "" {
		return nil, trace.BadParameter("misssing cert extension %v", utils.CertExtensionRole)
	}
	roles, err := teleport.ParseRoles(roleString)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	foundRoles := len(roles)
	if foundRoles != 1 {
		return nil, trace.Errorf("expected one role per certificate. found %d: '%s'",
			foundRoles, roles.String())
	}
	role := roles[0]
	authorityDomain := cert.Permissions.Extensions[utils.CertExtensionAuthority]
	if authorityDomain == "" {
		return nil, trace.BadParameter("misssing cert extension %v", utils.CertExtensionAuthority)
	}

	return &Identity{
		ID:              IdentityID{HostUUID: cert.ValidPrincipals[0], Role: role},
		AuthorityDomain: authorityDomain,
		KeyBytes:        keyBytes,
		CertBytes:       certBytes,
		KeySigner:       certSigner,
		Cert:            cert,
	}, nil
}

// ReadIdentity reads, parses and returns the given pub/pri key + cert from the
// key storage (dataDir).
func ReadIdentity(dataDir string, id IdentityID) (i *Identity, err error) {
	kp, cp := keysPath(dataDir, id)
	log.Debugf("host identity: [key: %v, cert: %v]", kp, cp)

	keyBytes, err := utils.ReadPath(kp)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	certBytes, err := utils.ReadPath(cp)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return ReadIdentityFromKeyPair(keyBytes, certBytes)
}

// WriteIdentity writes identity keypair to disk
func WriteIdentity(dataDir string, identity *Identity) error {
	return trace.Wrap(
		writeKeys(dataDir, identity.ID, identity.KeyBytes, identity.CertBytes))
}

// HaveHostKeys checks either the host keys are in place
func HaveHostKeys(dataDir string, id IdentityID) (bool, error) {
	kp, cp := keysPath(dataDir, id)

	exists, err := pathExists(kp)
	if !exists || err != nil {
		return exists, err
	}

	exists, err = pathExists(cp)
	if !exists || err != nil {
		return exists, err
	}

	return true, nil
}

// keysPath returns two full file paths: to the host.key and host.cert
func keysPath(dataDir string, id IdentityID) (key string, cert string) {
	return filepath.Join(dataDir, fmt.Sprintf("%s.key", strings.ToLower(string(id.Role)))),
		filepath.Join(dataDir, fmt.Sprintf("%s.cert", strings.ToLower(string(id.Role))))
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
