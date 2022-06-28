// Copyright 2017-2020 Lei Ni (nilei81@gmail.com) and other contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
//
// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
//
// Peer.go is the interface used by the upper layer to access functionalities
// provided by the raft protocol. It translates all incoming requests to raftpb
// messages and pass them to the raft protocol implementation to be handled.
// Such a state machine style design together with the iterative style interface
// here is derived from etcd.
// Compared to etcd raft, we strictly model all inputs to the raft protocol as
// messages including those used to advance the raft state.
//

package raft

import (
	"sort"

	"github.com/lni/dragonboat/v4/config"
	"github.com/lni/dragonboat/v4/internal/server"
	pb "github.com/lni/dragonboat/v4/raftpb"
)

// PeerAddress is the basic info for a peer in the Raft shard.
type PeerAddress struct {
	Address   string
	ReplicaID uint64
}

// Peer is the interface struct for interacting with the underlying Raft
// protocol implementation.
type Peer struct {
	raft      *raft
	prevState pb.State
}

// Launch starts or restarts a Raft node.
func Launch(config config.Config,
	logdb ILogDB, events server.IRaftEventListener,
	addresses []PeerAddress, initial bool, newNode bool) Peer {
	checkLaunchRequest(config, addresses, initial, newNode)
	plog.Infof("%s created, initial: %t, new: %t",
		dn(config.ShardID, config.ReplicaID), initial, newNode)
	p := Peer{raft: newRaft(config, logdb)}
	p.raft.events = events
	p.prevState = p.raft.raftState()
	if initial && newNode {
		p.raft.becomeFollower(1, NoLeader)
		bootstrap(p.raft, addresses)
	}
	return p
}

// Tick moves the logical clock forward by one tick.
func (p *Peer) Tick() error {
	return p.raft.Handle(pb.Message{
		Type:   pb.LocalTick,
		Reject: false,
	})
}

// QuiescedTick moves the logical clock forward by one tick in quiesced mode.
func (p *Peer) QuiescedTick() error {
	return p.raft.Handle(pb.Message{
		Type:   pb.LocalTick,
		Reject: true,
	})
}

func (p *Peer) QueryRaftLog(firstIndex uint64,
	lastIndex uint64, maxSize uint64) error {
	return p.raft.Handle(pb.Message{
		Type: pb.LogQuery,
		From: firstIndex,
		To:   lastIndex,
		Hint: maxSize,
	})
}

// RequestLeaderTransfer makes a request to transfer the leadership to the
// specified target node.
func (p *Peer) RequestLeaderTransfer(target uint64) error {
	return p.raft.Handle(pb.Message{
		Type: pb.LeaderTransfer,
		To:   p.raft.replicaID,
		Hint: target,
	})
}

// ProposeEntries proposes specified entries in a batched mode using a single
// MTPropose message.
func (p *Peer) ProposeEntries(ents []pb.Entry) error {
	return p.raft.Handle(pb.Message{
		Type:    pb.Propose,
		From:    p.raft.replicaID,
		Entries: ents,
	})
}

// ProposeConfigChange proposes a raft membership change.
func (p *Peer) ProposeConfigChange(cc pb.ConfigChange, key uint64) error {
	data := pb.MustMarshal(&cc)
	return p.raft.Handle(pb.Message{
		Type:    pb.Propose,
		Entries: []pb.Entry{{Type: pb.ConfigChangeEntry, Cmd: data, Key: key}},
	})
}

// ApplyConfigChange applies a raft membership change to the local raft node.
func (p *Peer) ApplyConfigChange(cc pb.ConfigChange) error {
	if cc.ReplicaID == NoLeader {
		p.raft.clearPendingConfigChange()
		return nil
	}
	return p.raft.Handle(pb.Message{
		Type:     pb.ConfigChangeEvent,
		Reject:   false,
		Hint:     cc.ReplicaID,
		HintHigh: uint64(cc.Type),
	})
}

// RejectConfigChange rejects the currently pending raft membership change.
func (p *Peer) RejectConfigChange() error {
	return p.raft.Handle(pb.Message{
		Type:   pb.ConfigChangeEvent,
		Reject: true,
	})
}

// RestoreRemotes applies the remotes info obtained from the specified snapshot.
func (p *Peer) RestoreRemotes(ss pb.Snapshot) error {
	return p.raft.Handle(pb.Message{
		Type:     pb.SnapshotReceived,
		Snapshot: ss,
	})
}

// ReportUnreachableNode marks the specified node as not reachable.
func (p *Peer) ReportUnreachableNode(replicaID uint64) error {
	return p.raft.Handle(pb.Message{
		Type: pb.Unreachable,
		From: replicaID,
	})
}

// ReportSnapshotStatus reports the status of the snapshot to the local raft
// node.
func (p *Peer) ReportSnapshotStatus(replicaID uint64, reject bool) error {
	return p.raft.Handle(pb.Message{
		Type:   pb.SnapshotStatus,
		From:   replicaID,
		Reject: reject,
	})
}

// Handle processes the given message.
func (p *Peer) Handle(m pb.Message) error {
	if IsLocalMessageType(m.Type) {
		panic("local message sent to Step")
	}
	_, rok := p.raft.remotes[m.From]
	_, ook := p.raft.nonVotings[m.From]
	_, wok := p.raft.witnesses[m.From]
	if rok || ook || wok || !isResponseMessageType(m.Type) {
		return p.raft.Handle(m)
	}
	return nil
}

// GetUpdate returns the current state of the Peer.
func (p *Peer) GetUpdate(moreToApply bool,
	lastApplied uint64) (pb.Update, error) {
	ud, err := p.getUpdate(moreToApply, lastApplied)
	if err != nil {
		return pb.Update{}, err
	}
	validateUpdate(ud)
	ud = setFastApply(ud)
	ud.UpdateCommit = getUpdateCommit(ud)
	return ud, nil
}

func setFastApply(ud pb.Update) pb.Update {
	ud.FastApply = true
	if !pb.IsEmptySnapshot(ud.Snapshot) {
		ud.FastApply = false
	}
	if ud.FastApply {
		if len(ud.CommittedEntries) > 0 && len(ud.EntriesToSave) > 0 {
			lastApplyIndex := ud.CommittedEntries[len(ud.CommittedEntries)-1].Index
			lastSaveIndex := ud.EntriesToSave[len(ud.EntriesToSave)-1].Index
			firstSaveIndex := ud.EntriesToSave[0].Index
			if lastApplyIndex >= firstSaveIndex && lastApplyIndex <= lastSaveIndex {
				ud.FastApply = false
			}
		}
	}
	return ud
}

func validateUpdate(ud pb.Update) {
	if ud.Commit > 0 && len(ud.CommittedEntries) > 0 {
		lastIndex := ud.CommittedEntries[len(ud.CommittedEntries)-1].Index
		if lastIndex > ud.Commit {
			plog.Panicf("trying to apply not committed entry: %d, %d",
				ud.Commit, lastIndex)
		}
	}
	if len(ud.CommittedEntries) > 0 && len(ud.EntriesToSave) > 0 {
		lastApply := ud.CommittedEntries[len(ud.CommittedEntries)-1].Index
		lastSave := ud.EntriesToSave[len(ud.EntriesToSave)-1].Index
		if lastApply > lastSave {
			plog.Panicf("trying to apply not saved entry: %d, %d",
				lastApply, lastSave)
		}
	}
}

// RateLimited returns a boolean flag indicating whether the Raft node is rate
// limited.
func (p *Peer) RateLimited() bool {
	return p.raft.rl.RateLimited()
}

// HasUpdate returns a boolean value indicating whether there is any Update
// ready to be processed.
func (p *Peer) HasUpdate(moreToApply bool) bool {
	r := p.raft
	if len(r.log.entriesToSave()) > 0 {
		return true
	}
	if r.logQueryResult != nil {
		return true
	}
	if r.leaderUpdate != nil {
		return true
	}
	if len(r.msgs) > 0 {
		return true
	}
	if moreToApply && r.log.hasEntriesToApply() {
		return true
	}
	if pst := r.raftState(); !pb.IsEmptyState(pst) &&
		!pb.IsStateEqual(pst, p.prevState) {
		return true
	}
	if r.log.inmem.snapshot != nil &&
		!pb.IsEmptySnapshot(*r.log.inmem.snapshot) {
		return true
	}
	if len(r.readyToRead) != 0 {
		return true
	}
	if len(r.droppedEntries) > 0 {
		return true
	}
	if len(r.droppedReadIndexes) > 0 {
		return true
	}
	return false
}

// Commit commits the Update state to mark it as processed.
func (p *Peer) Commit(ud pb.Update) {
	p.raft.msgs = nil
	p.raft.logQueryResult = nil
	p.raft.leaderUpdate = nil
	p.raft.droppedEntries = nil
	p.raft.droppedReadIndexes = nil
	if !pb.IsEmptyState(ud.State) {
		p.prevState = ud.State
	}
	if ud.UpdateCommit.ReadyToRead > 0 {
		p.raft.clearReadyToRead()
	}
	p.entryLog().commitUpdate(ud.UpdateCommit)
}

// ReadIndex starts a ReadIndex operation. The ReadIndex protocol is defined in
// the section 6.4 of the Raft thesis.
func (p *Peer) ReadIndex(ctx pb.SystemCtx) error {
	return p.raft.Handle(pb.Message{
		Type:     pb.ReadIndex,
		Hint:     ctx.Low,
		HintHigh: ctx.High,
	})
}

// NotifyRaftLastApplied passes on the lastApplied index confirmed by the RSM to
// the raft state machine.
func (p *Peer) NotifyRaftLastApplied(lastApplied uint64) {
	p.raft.setApplied(lastApplied)
}

// HasEntryToApply returns a boolean flag indicating whether there are more
// entries ready to be applied.
func (p *Peer) HasEntryToApply() bool {
	return p.entryLog().hasEntriesToApply()
}

func (p *Peer) entryLog() *entryLog {
	return p.raft.log
}

func (p *Peer) getUpdate(moreToApply bool,
	lastApplied uint64) (pb.Update, error) {
	ud := pb.Update{
		ShardID:       p.raft.shardID,
		ReplicaID:     p.raft.replicaID,
		EntriesToSave: p.entryLog().entriesToSave(),
		Messages:      p.raft.msgs,
		LastApplied:   lastApplied,
		FastApply:     true,
	}
	if p.raft.logQueryResult != nil {
		ud.LogQueryResult = *p.raft.logQueryResult
	}
	if p.raft.leaderUpdate != nil {
		ud.LeaderUpdate = *p.raft.leaderUpdate
	}
	for idx := range ud.Messages {
		ud.Messages[idx].ShardID = p.raft.shardID
	}
	if moreToApply {
		toApply, err := p.entryLog().entriesToApply()
		if err != nil {
			return pb.Update{}, err
		}
		ud.CommittedEntries = toApply
	}
	if len(ud.CommittedEntries) > 0 {
		lastIndex := ud.CommittedEntries[len(ud.CommittedEntries)-1].Index
		ud.MoreCommittedEntries = p.entryLog().hasMoreEntriesToApply(lastIndex)
	}
	if pst := p.raft.raftState(); !pb.IsStateEqual(pst, p.prevState) {
		ud.State = pst
	}
	if p.entryLog().inmem.snapshot != nil {
		ud.Snapshot = *p.entryLog().inmem.snapshot
	}
	if len(p.raft.readyToRead) > 0 {
		ud.ReadyToReads = p.raft.readyToRead
	}
	if len(p.raft.droppedEntries) > 0 {
		ud.DroppedEntries = p.raft.droppedEntries
	}
	if len(p.raft.droppedReadIndexes) > 0 {
		ud.DroppedReadIndexes = p.raft.droppedReadIndexes
	}
	return ud, nil
}

func checkLaunchRequest(config config.Config,
	addresses []PeerAddress, initial bool, newNode bool) {
	if config.ReplicaID == 0 {
		panic("config.ReplicaID must not be zero")
	}
	if initial && newNode && len(addresses) == 0 {
		panic("addresses must be specified")
	}
	uniqueAddressList := make(map[string]struct{})
	for _, addr := range addresses {
		uniqueAddressList[addr.Address] = struct{}{}
	}
	if len(uniqueAddressList) != len(addresses) {
		plog.Panicf("duplicated address found %v", addresses)
	}
	if initial && config.IsWitness {
		plog.Panicf("witness can not be used as initial member")
	}
	if initial && config.IsNonVoting {
		plog.Panicf("non-voting can not be used as initial member")
	}
}

func bootstrap(r *raft, addresses []PeerAddress) {
	sort.Slice(addresses, func(i, j int) bool {
		return addresses[i].ReplicaID < addresses[j].ReplicaID
	})
	ents := make([]pb.Entry, len(addresses))
	for i, peer := range addresses {
		plog.Infof("%s added bootstrap ConfigChangeAddNode, %d, %s",
			r.describe(), peer.ReplicaID, peer.Address)
		cc := pb.ConfigChange{
			Type:       pb.AddNode,
			ReplicaID:  peer.ReplicaID,
			Initialize: true,
			Address:    peer.Address,
		}
		ents[i] = pb.Entry{
			Type:  pb.ConfigChangeEntry,
			Term:  1,
			Index: uint64(i + 1),
			Cmd:   pb.MustMarshal(&cc),
		}
	}
	r.log.append(ents)
	r.log.committed = uint64(len(ents))
	for _, peer := range addresses {
		r.addNode(peer.ReplicaID)
	}
}

func getUpdateCommit(ud pb.Update) pb.UpdateCommit {
	uc := pb.UpdateCommit{
		ReadyToRead: uint64(len(ud.ReadyToReads)),
		LastApplied: ud.LastApplied,
	}
	if len(ud.CommittedEntries) > 0 {
		uc.Processed = ud.CommittedEntries[len(ud.CommittedEntries)-1].Index
	}
	if len(ud.EntriesToSave) > 0 {
		lastEntry := ud.EntriesToSave[len(ud.EntriesToSave)-1]
		uc.StableLogTo, uc.StableLogTerm = lastEntry.Index, lastEntry.Term
	}
	if !pb.IsEmptySnapshot(ud.Snapshot) {
		uc.StableSnapshotTo = ud.Snapshot.Index
		uc.Processed = max(uc.Processed, uc.StableSnapshotTo)
	}
	return uc
}
