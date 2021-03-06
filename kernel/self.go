package kernel

import (
	"sort"
	"time"

	"github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/config"
	"github.com/MixinNetwork/mixin/crypto"
)

func (node *Node) checkCacheSnapshotTransaction(s *common.Snapshot) (*common.SignedTransaction, error) {
	inNode, err := node.store.CheckTransactionInNode(s.NodeId, s.Transaction)
	if err != nil || inNode {
		return nil, err
	}

	finalized, err := node.store.CheckTransactionFinalization(s.Transaction)
	if err != nil || finalized {
		return nil, err
	}

	tx, err := node.store.ReadTransaction(s.Transaction)
	if err != nil || tx != nil {
		return tx, err
	}

	tx, err = node.store.CacheGetTransaction(s.Transaction)
	if err != nil || tx == nil {
		return nil, err
	}

	if tx.CheckMint() {
		err = node.validateMintTransaction(tx)
		if err != nil {
			return nil, nil
		}
	}

	err = tx.Validate(node.store)
	if err != nil {
		return nil, nil
	}

	err = tx.LockInputs(node.store, false)
	if err != nil {
		return nil, err
	}

	return tx, node.store.WriteTransaction(tx)
}

func (node *Node) collectSelfSignatures(s *common.Snapshot) error {
	if s.NodeId != node.IdForNetwork || len(s.Signatures) != 1 {
		panic("should never be here")
	}
	if len(node.SnapshotsPool[s.Hash]) == 0 || node.SignaturesPool[s.Hash] == nil {
		return nil
	}

	cache := node.Graph.CacheRound[s.NodeId].Copy()
	if s.RoundNumber < cache.Number {
		return node.clearAndQueueSnapshotOrPanic(s)
	}
	if !cache.ValidateSnapshot(s, false) {
		return node.clearAndQueueSnapshotOrPanic(s)
	}

	filter := make(map[string]bool)
	osigs := node.SnapshotsPool[s.Hash]
	for _, sig := range osigs {
		filter[sig.String()] = true
	}
	for _, sig := range s.Signatures {
		if filter[sig.String()] {
			continue
		}
		osigs = append(osigs, sig)
		filter[sig.String()] = true
	}
	node.SnapshotsPool[s.Hash] = append([]*crypto.Signature{}, osigs...)

	if !node.verifyFinalization(osigs) {
		return nil
	}

	s.Signatures = append([]*crypto.Signature{}, osigs...)
	topo := &common.SnapshotWithTopologicalOrder{
		Snapshot:         *s,
		TopologicalOrder: node.TopoCounter.Next(),
	}
	err := node.store.WriteSnapshot(topo)
	if err != nil {
		panic(err)
	}
	if !cache.ValidateSnapshot(s, true) {
		panic("should never be here")
	}
	node.Graph.CacheRound[s.NodeId] = cache
	node.removeFromCache(s)

	for peerId, _ := range node.ConsensusNodes {
		err := node.Peer.SendSnapshotMessage(peerId, s, 1)
		if err != nil {
			return err
		}
	}
	return nil
}

func (node *Node) determinBestRound() *FinalRound {
	var best *FinalRound
	var start, height uint64
	for id, rounds := range node.Graph.RoundHistory {
		if rc := len(rounds) - config.SnapshotReferenceThreshold; rc > 0 {
			rounds = append([]*FinalRound{}, rounds[rc:]...)
		}
		node.Graph.RoundHistory[id] = rounds
		if id != node.IdForNetwork && best == nil {
			best = rounds[0]
		}
		rts, rh := rounds[0].Start, uint64(len(rounds))
		if id == node.IdForNetwork || rh < height {
			continue
		}
		if rts+config.SnapshotRoundGap*rh > uint64(time.Now().UnixNano()) {
			continue
		}
		if rh > height || rts > start {
			best = rounds[0]
			start, height = rts, rh
		}
	}
	return best
}

func (node *Node) signSelfSnapshot(s *common.Snapshot, tx *common.SignedTransaction) error {
	if s.NodeId != node.IdForNetwork || len(s.Signatures) != 0 || s.Timestamp != 0 {
		panic("should never be here")
	}

	cache := node.Graph.CacheRound[s.NodeId].Copy()
	final := node.Graph.FinalRound[s.NodeId].Copy()

	if !node.checkCacheCapability() {
		time.Sleep(10 * time.Millisecond)
		return node.queueSnapshotOrPanic(s, false)
	}
	if !node.CheckSync() && len(cache.Snapshots) == 0 {
		time.Sleep(time.Duration(config.SnapshotRoundGap / 2))
		return node.queueSnapshotOrPanic(s, false)
	}

	for {
		s.Timestamp = uint64(time.Now().UnixNano())
		if s.Timestamp > cache.Timestamp {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	if start, _ := cache.Gap(); s.Timestamp >= start+config.SnapshotRoundGap {
		best := node.determinBestRound()
		if best.NodeId == final.NodeId {
			panic("should never be here")
		}

		final = cache.asFinal()
		cache = &CacheRound{
			NodeId: s.NodeId,
			Number: final.Number + 1,
			References: &common.RoundLink{
				Self:     final.Hash,
				External: best.Hash,
			},
		}
		err := node.store.StartNewRound(cache.NodeId, cache.Number, cache.References, final.Start)
		if err != nil {
			panic(err)
		}
		node.CachePool = make([]*common.Snapshot, 0)
	}
	cache.Timestamp = s.Timestamp

	s.RoundNumber = cache.Number
	s.References = cache.References
	node.Graph.CacheRound[s.NodeId] = cache
	node.Graph.FinalRound[s.NodeId] = final
	node.Graph.RoundHistory[s.NodeId] = append(node.Graph.RoundHistory[s.NodeId], final.Copy())
	node.signSnapshot(s)
	s.Signatures = []*crypto.Signature{node.SignaturesPool[s.Hash]}
	for peerId, _ := range node.ConsensusNodes {
		err := node.Peer.SendTransactionMessage(peerId, tx)
		if err != nil {
			return err
		}
		err = node.Peer.SendSnapshotMessage(peerId, s, 0)
		if err != nil {
			return err
		}
	}
	node.CachePool = append(node.CachePool, s)
	return nil
}

func (node *Node) checkCacheCapability() bool {
	count := len(node.CachePool)
	if count == 0 {
		return true
	}
	sort.Slice(node.CachePool, func(i, j int) bool {
		return node.CachePool[i].Timestamp < node.CachePool[j].Timestamp
	})
	start := node.CachePool[0].Timestamp
	end := node.CachePool[count-1].Timestamp
	if uint64(time.Now().UnixNano()) >= start+config.SnapshotRoundGap*3/2 {
		return true
	}
	return end < start+config.SnapshotRoundGap/3*2
}

func (node *Node) removeFromCache(s *common.Snapshot) {
	for i, c := range node.CachePool {
		if c.Hash != s.Hash {
			continue
		}
		l := len(node.CachePool)
		node.CachePool[l-1], node.CachePool[i] = node.CachePool[i], node.CachePool[l-1]
		node.CachePool = node.CachePool[:l-1]
		return
	}
}
