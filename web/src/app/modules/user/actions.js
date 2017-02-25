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

import reactor from 'app/reactor';
import { TLPT_RECEIVE_USER, TLPT_RECEIVE_USER_INVITE } from './actionTypes';
import { TRYING_TO_SIGN_UP, TRYING_TO_LOGIN, FETCHING_INVITE} from 'app/modules/restApi/constants';
import restApiActions from 'app/modules/restApi/actions';
import auth from 'app/services/auth';
import cfg from 'app/config';
import api from 'app/services/api';

const actions = {

  fetchInvite(inviteToken){
    let path = cfg.api.getInviteUrl(inviteToken);
    restApiActions.start(FETCHING_INVITE);    
    api.get(path).done(invite=>{
      restApiActions.success(FETCHING_INVITE);
      reactor.dispatch(TLPT_RECEIVE_USER_INVITE, invite);
    })
    .fail(err => {
      let msg = api.getErrorText(err);        
      restApiActions.fail(FETCHING_INVITE, msg);
    });
  },

  ensureUser(nextState, replace, cb){
    auth.ensureUser()
      .done((userData)=> {
        reactor.dispatch(TLPT_RECEIVE_USER, userData.user );
        cb();
      })
      .fail(()=>{
        let newLocation = {
          pathname: cfg.routes.login,
          state: {
            redirectTo: nextState.location.pathname
          }
        };

        replace(newLocation);
        cb();
      });
  },

  signup(name, psw, token, inviteToken){    
    let promise = auth.signUp(name, psw, token, inviteToken);
    actions._handleSignupPromise(promise);
  },

  signupWithU2f(name, psw, inviteToken) {
    let promise = auth.signUpWithU2f(name, psw, inviteToken);
    actions._handleSignupPromise(promise);
  },

  signupWithOidc(provider, token) {
    let redirectUrl = cfg.getFullUrl(cfg.routes.app);
    let url = cfg.api.getInviteWithOidcUrl(token, provider, redirectUrl);
    auth.redirect(url);
  },

  loginWithOidc(provider, redirect) {
    let fullPath = cfg.getFullUrl(redirect);
    auth.redirect(cfg.api.getSsoUrl(fullPath, provider));    
  },

  loginWithU2f(user, password, redirect) {
    let promise = auth.loginWithU2f(user, password);
    actions._handleLoginPromise(promise, redirect);
  },

  login(user, password, token, redirect) {
    let promise = auth.login(user, password, token);
    actions._handleLoginPromise(promise, redirect);              
  },

  _handleSignupPromise(promise) {
    restApiActions.start(TRYING_TO_SIGN_UP);    
    promise
      .done(() => {                
        auth.redirect(cfg.routes.app);        
      })
      .fail(err => {
        let msg = api.getErrorText(err);        
        restApiActions.fail(TRYING_TO_SIGN_UP, msg);
      })        
  },

  _handleLoginPromise(promise, redirect) {
    restApiActions.start(TRYING_TO_LOGIN);
    promise
      .done(() => {        
        auth.redirect(redirect);        
      })
      .fail(err => {
        let msg = api.getErrorText(err);
        restApiActions.fail(TRYING_TO_LOGIN, msg);
      })
  }
}

export default actions;
