/*
Copyright IBM Corp. 2017 All Rights Reserved.

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

package blocksprovider

import (
	"sync/atomic"

	"github.com/golang/protobuf/proto"
	gossipcommon "github.com/hyperledger/fabric/gossip/common"
	"github.com/hyperledger/fabric/gossip/discovery"

	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/gossip/api"
	"github.com/hyperledger/fabric/protos/common"
	gossip_proto "github.com/hyperledger/fabric/protos/gossip"
	"github.com/hyperledger/fabric/protos/orderer"
	"github.com/op/go-logging"
)

// LedgerInfo an adapter to provide the interface to query
// the ledger committer for current ledger height
type LedgerInfo interface {
	// LedgerHeight returns current local ledger height
	LedgerHeight() (uint64, error)
}

// GossipServiceAdapter serves to provide basic functionality
// required from gossip service by delivery service
type GossipServiceAdapter interface {
	// PeersOfChannel returns slice with members of specified channel
	PeersOfChannel(gossipcommon.ChainID) []discovery.NetworkMember

	// AddPayload adds payload to the local state sync buffer
	AddPayload(chainID string, payload *gossip_proto.Payload) error

	// Gossip the message across the peers
	Gossip(msg *gossip_proto.GossipMessage)
}

// BlocksProvider used to read blocks from the ordering service
// for specified chain it subscribed to
type BlocksProvider interface {
	// DeliverBlocks starts delivering and disseminating blocks
	DeliverBlocks()

	// Stop shutdowns blocks provider and stops delivering new blocks
	Stop()
}

// BlocksDeliverer defines interface which actually helps
// to abstract the AtomicBroadcast_DeliverClient with only
// required method for blocks provider.
// This also decouples the production implementation of the gRPC stream
// from the code in order for the code to be more modular and testable.
type BlocksDeliverer interface {
	// Recv retrieves a response from the ordering service
	Recv() (*orderer.DeliverResponse, error)

	// Send sends an envelope to the ordering service
	Send(*common.Envelope) error
}

type streamClient interface {
	BlocksDeliverer

	// Close closes the stream and its underlying connection
	Close()
}

// blocksProviderImpl the actual implementation for BlocksProvider interface
type blocksProviderImpl struct {
	chainID string

	client streamClient

	gossip GossipServiceAdapter

	mcs api.MessageCryptoService

	done int32
}

var logger *logging.Logger // package-level logger

func init() {
	logger = flogging.MustGetLogger("blocksProvider")
}

// NewBlocksProvider constructor function to create blocks deliverer instance
func NewBlocksProvider(chainID string, client streamClient, gossip GossipServiceAdapter, mcs api.MessageCryptoService) BlocksProvider {
	return &blocksProviderImpl{
		chainID: chainID,
		client:  client,
		gossip:  gossip,
		mcs:     mcs,
	}
}

// DeliverBlocks used to pull out blocks from the ordering service to
// distributed them across peers
func (b *blocksProviderImpl) DeliverBlocks() {
	defer b.client.Close()
	for !b.isDone() {
		msg, err := b.client.Recv()
		if err != nil {
			logger.Warningf("Receive error: %s", err.Error())
			return
		}
		switch t := msg.Type.(type) {
		case *orderer.DeliverResponse_Status:
			if t.Status == common.Status_SUCCESS {
				logger.Warning("ERROR! Received success for a seek that should never complete")
				return
			}
			logger.Warning("Got error ", t)
		case *orderer.DeliverResponse_Block:
			seqNum := t.Block.Header.Number

			marshaledBlock, err := proto.Marshal(t.Block)
			if err != nil {
				logger.Errorf("Error serializing block with sequence number %d, due to %s", seqNum, err)
				continue
			}
			if err := b.mcs.VerifyBlock(gossipcommon.ChainID(b.chainID), seqNum, marshaledBlock); err != nil {
				logger.Errorf("Error verifying block with sequnce number %d, due to %s", seqNum, err)
				continue
			}

			numberOfPeers := len(b.gossip.PeersOfChannel(gossipcommon.ChainID(b.chainID)))
			// Create payload with a block received
			payload := createPayload(seqNum, marshaledBlock)
			// Use payload to create gossip message
			gossipMsg := createGossipMsg(b.chainID, payload)

			logger.Debugf("Adding payload locally, buffer seqNum = [%d], peers number [%d]", seqNum, numberOfPeers)
			// Add payload to local state payloads buffer
			b.gossip.AddPayload(b.chainID, payload)

			// Gossip messages with other nodes
			logger.Debugf("Gossiping block [%d], peers number [%d]", seqNum, numberOfPeers)
			b.gossip.Gossip(gossipMsg)
		default:
			logger.Warning("Received unknown: ", t)
			return
		}
	}
}

// Stop stops blocks delivery provider
func (b *blocksProviderImpl) Stop() {
	atomic.StoreInt32(&b.done, 1)
	b.client.Close()
}

// Check whenever provider is stopped
func (b *blocksProviderImpl) isDone() bool {
	return atomic.LoadInt32(&b.done) == 1
}

func createGossipMsg(chainID string, payload *gossip_proto.Payload) *gossip_proto.GossipMessage {
	gossipMsg := &gossip_proto.GossipMessage{
		Nonce:   0,
		Tag:     gossip_proto.GossipMessage_CHAN_AND_ORG,
		Channel: []byte(chainID),
		Content: &gossip_proto.GossipMessage_DataMsg{
			DataMsg: &gossip_proto.DataMessage{
				Payload: payload,
			},
		},
	}
	return gossipMsg
}

func createPayload(seqNum uint64, marshaledBlock []byte) *gossip_proto.Payload {
	return &gossip_proto.Payload{
		Data:   marshaledBlock,
		SeqNum: seqNum,
	}
}
