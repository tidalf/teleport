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

import React from 'react';
import reactor from 'app/reactor';
import userGetters from 'app/modules/user/getters';
import nodeGetters from 'app/modules/nodes/getters';
import NodeList from './nodeList.jsx';

const Nodes = React.createClass({

  mixins: [reactor.ReactMixin],

  getDataBindings() {
    return {      
      nodeRecords: nodeGetters.nodeListView,
      user: userGetters.user            
    }
  },

  render() {
    let { nodeRecords, user, sites, siteId } = this.state;
    let { logins } = user;
    return (   
      <div className="grv-page">
        <NodeList
          siteId={siteId}
          sites={sites} 
          nodeRecords={nodeRecords} 
          logins={logins}
        />
      </div>
    );
  }  
});

export default Nodes;
