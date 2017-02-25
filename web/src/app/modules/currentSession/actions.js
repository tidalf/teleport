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
import session from 'app/services/session';
import api from 'app/services/api';
import cfg from 'app/config';
import getters from './getters';
import { fetchStoredSession, updateSession } from './../sessions/actions';
import sessionGetters from './../sessions/getters';
import {showError} from 'app/modules/notifications/actions';

const logger = require('app/common/logger').create('Current Session');

const { TLPT_CURRENT_SESSION_OPEN, TLPT_CURRENT_SESSION_CLOSE } = require('./actionTypes');

const actions = {

  createNewSession(siteId, serverId, login) {
    let data = {
      session: {
        terminal_params: {
          w: 45,
          h: 5
        },
        login
      }
    }

    api.post(cfg.api.getSiteSessionUrl(siteId), data).then(json => {
      let history = session.getHistory();
      let sid = json.session.id;
      let routeUrl = cfg.getCurrentSessionRouteUrl({
        siteId,
        sid
      });
            
      reactor.dispatch(TLPT_CURRENT_SESSION_OPEN, {
        siteId,
        serverId,
        login,
        sid,
        active: true,
        isNew: true
      });

      history.push(routeUrl);
    });
  },

  initSession(sid) {
    logger.info('attempt to open session', { sid });
    
    // check if terminal is already open
    let currentSession = reactor.evaluate(getters.currentSession);
    if (currentSession) {
      return;
    }
          
    // look up active session matching given sid
    let activeSession = reactor.evaluate(sessionGetters.activeSessionById(sid));
    if (activeSession) {
      let { server_id, login, siteId, id } = activeSession;
      reactor.dispatch(TLPT_CURRENT_SESSION_OPEN, {                
        login,        
        siteId,
        sid: id,
        serverId: server_id,
        active: true
      });
        
      return;
    }

    // stored session then...      
    fetchStoredSession(sid)
      .done(() => {
        let storedSession = reactor.evaluate(sessionGetters.storedSessionById(sid));
        if (!storedSession) {
          // TODO: display not found page
          showError('Cannot find archived session'); 
          return;
        }

        let { siteId } = storedSession;    
        reactor.dispatch(TLPT_CURRENT_SESSION_OPEN, {
          siteId,          
          sid          
        });
          
      })
      .fail(err => {          
        let msg = err.responseJSON ? err.responseJSON.message : '';
        showError('Unable to fetch archived session', msg);    
        logger.error('open session', err);
      })
  },

  close() {
    let { isNew } = reactor.evaluate(getters.currentSession);

    reactor.dispatch(TLPT_CURRENT_SESSION_CLOSE);

    if (isNew) {
      session.getHistory().push(cfg.routes.nodes);
    } else {
      session.getHistory().push(cfg.routes.sessions);
    }
  },

  updateSessionFromEventStream(siteId) {
    return data => {
      data.events.forEach(item => {
        if (item.event === 'session.end') {
          actions.close();
        }
      })
      
      updateSession({
        siteId: siteId,
        json: data.session
      });
    }
  }

}

export default actions;