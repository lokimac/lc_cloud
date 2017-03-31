// Copyright 2015 refractionPOINT
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lcServer

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/refractionPOINT/lc_cloud/standalone/termination_server/rpcm"
	"github.com/refractionPOINT/lc_cloud/standalone/termination_server/utils"
	"io/ioutil"
	"sync"
	"time"
)

type Client interface {
	ProcessIncoming(moduleID uint8, messages []*rpcm.Sequence) error
	AgentID() hcp.AgentID
	Stop() error
}

type client struct {
	sendMu sync.Mutex
	recvMu sync.Mutex
	srv *server
	isOnline bool
	isAuthenticated bool
	aID hcp.AgentID
	sendCB func(moduleID uint8, messages []*rpcm.Sequence) error
	incomingChannel chan TelemetryMessage
}

func (c *client) send(moduleID uint8, messages []*rpcm.Sequence) error {
	defer c.sendMu.Unlock()
	c.sendMu.Lock()

	if c.isOnline && c.isAuthenticated {
		return c.sendCB(moduleID, messages)
	}

	return errors.New("not connected or authenticated")
}

func (c *client) sendTimeSync() error {
	messages := []*rpcm.Sequence{rpcm.NewSequence().
			AddInt8(rpcm.RP_TAGS_OPERATION, hcp.SET_GLOBAL_TIME).
			AddTimestamp(rpcm.RP_TAGS_TIMESTAMP, uint64(time.Now().Unix()))}
	return c.send(hcp.MODULE_ID_HCP, messages)
}

func (c *client) AgentID() hcp.AgentID {
	return c.aID
}

func (c *client) setAgentID(aID hcp.AgentID) {
	c.aID = aID
}

func (c *client) ProcessIncoming(moduleID uint8, messages []*rpcm.Sequence) error {
	defer c.recvMu.Unlock()
	c.recvMu.Lock()

	if c.srv.isClosing {
		return errors.New("server closing")
	}

	if !c.isOnline {
		return errors.New("not connected")
	}

	if !c.isAuthenticated {
		// Client must authenticate first
		if moduleID != hcp.MODULE_ID_HCP {
			return errors.New("expected authentication first")
		}

		if len(messages) == 0 {
			return errors.New("no authentication message in frame")
		}

		headers := messages[0]
		hostName, _ := headers.GetStringA(rpcm.RP_TAGS_HOST_NAME)
		internalIP, _ := headers.GetIPv4(rpcm.RP_TAGS_IP_ADDRESS)
		if tmpSeq, ok := headers.GetSequence(rpcm.RP_TAGS_HCP_IDENT); ok {
			if err := c.aID.FromSequence(tmpSeq); err != nil {
				return err
			}
		}

		if !c.aID.IsAbsolute() {
			return errors.New(fmt.Sprintf("invalid AID: %s", c.aID))
		}

		if c.aID.IsSIDWild() {
			// This sensor requires enrollment
			// We get the relevant enrollment messages (if elligible) from the server and
			// we send them directly via the CB since we know the sensor is not authenticated yet
			if enrollmentMessages, err := c.srv.enrollClient(c); err != nil {
				return err
			} else if err := c.sendCB(hcp.MODULE_ID_HCP, enrollmentMessages); err != nil {
				return err
			}
		} else {
			// This sensor should already be enrolled with a valid token
			sensorToken, ok := headers.GetBuffer(rpcm.RP_TAGS_HCP_ENROLLMENT_TOKEN)
			if !ok {
				return errors.New("client has no enrollment token")
			}
			if !c.srv.validateEnrollmentToken(c.aID, sensorToken) {
				return errors.New("invalid enrollment token")
			}
		}

		c.isAuthenticated = true

		// Upon authentication we send an initial clock sync
		if err := c.sendTimeSync(); err != nil {
			return err
		}

		if err := c.srv.setOnline(c, ConnectMessage{AID: &c.aID, Hostname: hostName, InternalIP: internalIP}); err != nil  {
			return err
		}
	}

	// By this point we are either a valid returning client or a newly enrolled one

	switch moduleID {
	case hcp.MODULE_ID_HCP:
		return c.processHCPMessage(messages)
	case hcp.MODULE_ID_HBS:
		return c.processHBSMessage(messages)
	default:
		return errors.New(fmt.Sprintf("received messages from unexpected module: %d", moduleID))
	}

	return nil
}

func (c *client) processHCPMessage(messages []*rpcm.Sequence) error {
	shouldBeLoaded := c.srv.getExpectedModulesFor(c.aID)

	var currentlyLoaded []ModuleRule
	for _, message := range messages {
		if modules, ok := message.GetList(rpcm.RP_TAGS_HCP_MODULES); ok {
			for _, moduleInfo := range modules.GetSequence(rpcm.RP_TAGS_HCP_MODULE) {
				var mod ModuleRule
				var ok bool
				if mod.Hash, ok = moduleInfo.GetBuffer(rpcm.RP_TAGS_HASH); !ok {
					return errors.New("module entry missing hash")
				}
				if mod.ModuleID, ok = moduleInfo.GetInt8(rpcm.RP_TAGS_HCP_MODULE_ID); !ok {
					return errors.New("module entry missing module ID")
				}

				currentlyLoaded = append(currentlyLoaded, mod)
			}
		}
	}

	outMessages := make([]*rpcm.Sequence, 0, 5)

	// Look for modules that should be unloaded
	for _, modIsLoaded := range currentlyLoaded {
		var isFound bool
		for _, modShouldBeLoaded := range shouldBeLoaded {
			if modIsLoaded.ModuleID == modShouldBeLoaded.ModuleID &&
				bytes.Equal(modIsLoaded.Hash, modShouldBeLoaded.Hash) {
				isFound = true
				break
			}
		}

		if !isFound {
			outMessages = append(outMessages, rpcm.NewSequence().
					AddInt8(rpcm.RP_TAGS_OPERATION, hcp.UNLOAD_MODULE).
					AddInt8(rpcm.RP_TAGS_HCP_MODULE_ID, modIsLoaded.ModuleID))
		}
	}

	// Look for modules that need to be loaded
	for _, modShouldBeLoaded := range shouldBeLoaded {
		var isFound bool
		for _, modIsLoaded := range currentlyLoaded {
			if modIsLoaded.ModuleID == modShouldBeLoaded.ModuleID &&
				bytes.Equal(modIsLoaded.Hash, modShouldBeLoaded.Hash) {
				isFound = true
				break
			}
		}

		if !isFound {
			var err error
			var moduleContent []byte
			var moduleSig []byte
			if moduleContent, err = ioutil.ReadFile(modShouldBeLoaded.ModuleFile); err != nil {
				return err
			}
			if moduleSig, err = ioutil.ReadFile(fmt.Sprintf("%s.sig", modShouldBeLoaded.ModuleFile)); err != nil {
				return err
			}

			outMessages = append(outMessages, rpcm.NewSequence().
					AddInt8(rpcm.RP_TAGS_OPERATION, hcp.LOAD_MODULE).
					AddInt8(rpcm.RP_TAGS_HCP_MODULE_ID, modShouldBeLoaded.ModuleID).
					AddBuffer(rpcm.RP_TAGS_BINARY, moduleContent).
					AddBuffer(rpcm.RP_TAGS_SIGNATURE, moduleSig))
		}
	}

	// We also take the sync opportunity to send a time sync
	outMessages = append(outMessages, rpcm.NewSequence().
			AddInt8(rpcm.RP_TAGS_OPERATION, hcp.SET_GLOBAL_TIME).
			AddTimestamp(rpcm.RP_TAGS_TIMESTAMP, uint64(time.Now().Unix())))
	
	return c.send(hcp.MODULE_ID_HCP, outMessages)
}

func (c *client) processHBSMessage(messages []*rpcm.Sequence) error {
	outMessages := make([]*rpcm.Sequence, 1)

	for _, message := range messages {
		if syncMessage, ok := message.GetSequence(rpcm.RP_TAGS_NOTIFICATION_SYNC); ok {
			var profile ProfileRule
			var ok bool
			if profile, ok = c.srv.getHBSProfileFor(c.aID); !ok {
				return errors.New("no HBS profile to load")
			}

			if currentProfileHash, ok := syncMessage.GetBuffer(rpcm.RP_TAGS_HASH); ok {
				if bytes.Equal(profile.Hash, currentProfileHash) {
					// The sensor already has the latest profile
					continue
				}
			}

			var profileContent []byte
			var err error
			if profileContent, err = ioutil.ReadFile(profile.ProfileFile); err != nil {
				return err
			}

			actualHash := sha256.Sum256(profileContent)

			if !bytes.Equal(actualHash[:], profile.Hash) {
				return errors.New(fmt.Sprintf("profile content seems to have changed"))
			}

			parsedProfile := rpcm.NewList(0, 0)
			if err = parsedProfile.Deserialize(bytes.NewBuffer(profileContent)); err != nil {
				return err
			}

			outMessages = append(outMessages, rpcm.NewSequence().
					AddSequence(rpcm.RP_TAGS_NOTIFICATION_SYNC, rpcm.NewSequence().
					AddBuffer(rpcm.RP_TAGS_HASH, profile.Hash).
					AddList(rpcm.RP_TAGS_HBS_CONFIGURATIONS, parsedProfile)))
		} else {
			var data TelemetryMessage
			data.Event = message
			data.AID = &c.aID
			c.srv.incomingChannel <- data
		}
	}

	return nil
}

func (c *client) Stop() error {
	defer c.sendMu.Unlock()
	defer c.recvMu.Unlock()
	c.sendMu.Lock()
	c.recvMu.Lock()

	if !c.isOnline {
		// Double close, ignore
		return nil
	}

	c.srv.disconnectChannel <- DisconnectMessage{AID: &c.aID}

	if c.isAuthenticated {
		c.srv.setOffline(c)
	}

	c.isAuthenticated = false
	c.isOnline = false

	return nil
}