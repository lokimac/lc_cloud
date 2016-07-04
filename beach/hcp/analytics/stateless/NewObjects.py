# Copyright 2015 refractionPOINT
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from beach.actor import Actor
ObjectTypes = Actor.importLib( '../../ObjectsDb', 'ObjectTypes' )
HostObjects = Actor.importLib( '../../ObjectsDb', 'HostObjects' )
StatelessActor = Actor.importLib( '../../Detects', 'StatelessActor' )
CassDb = Actor.importLib( '../../hcp_databases', 'CassDb' )
CassPool = Actor.importLib( '../../hcp_databases', 'CassPool' )

from sets import Set

class NewObjects ( StatelessActor ):
    def init( self, parameters ):
        super( NewObjects, self ).init( parameters )
        self.db = parameters[ 'db' ]
        self.typesOfInterest = Set( parameters.get( 'types', [] ) )
        self.typesOfInterest = map( lambda x: ObjectTypes.forward[ x ], self.typesOfInterest )
        self.hotCache = {}
        self.buildCache()

    def buildCache( self ):
        self.log( "Starting to build hot cache from scaling storage" )
        i = 0
        HostObjects.setDatabase( self.db )

        for _, o, oType in HostObjects.ofTypes( self.typesOfInterest ).info():
            self.hotCache.setdefault( oType, Set() ).add( o )
            i += 1

        HostObjects.closeDatabase()
        self.log( "Finished building hot cache (%d new objects)." % ( i, ) )

    def process( self, detects, msg ):
        routing, event, mtd = msg.data
        oRoot = mtd[ 'obj' ]
        
        for oType in self.typesOfInterest:
            for o in oRoot.get( oType, tuple() ):
                if o not in self.hotCache.setdefault( oType, Set() ):
                    self.hotCache[ oType ].add( o )
                    detects.add( 30,
                                 'new interesting object never seen before',
                                 { 'type' : ObjectTypes.rev[ oType ],
                                   'object' : o },
                                 None )
