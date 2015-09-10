package consensus

import (
	"errors"

	"github.com/boltdb/bolt"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"
)

var (
	errMissingFileContract = errors.New("storage proof submitted for non existing file contract")
	errOutputAlreadyMature = errors.New("delayed siacoin output is already in the matured outputs set")
	errPayoutsAlreadyPaid  = errors.New("payouts are already in the consensus set")
	errStorageProofTiming  = errors.New("missed proof triggered for file contract that is not expiring")
)

// applyMinerPayouts adds a block's miner payouts to the consensus set as
// delayed siacoin outputs.
func applyMinerPayouts(tx *bolt.Tx, pb *processedBlock) error {
	for i := range pb.Block.MinerPayouts {
		// Create and apply the delayed miner payout.
		mpid := pb.Block.MinerPayoutID(uint64(i))
		dscod := modules.DelayedSiacoinOutputDiff{
			Direction:      modules.DiffApply,
			ID:             mpid,
			SiacoinOutput:  pb.Block.MinerPayouts[i],
			MaturityHeight: pb.Height + types.MaturityDelay,
		}
		pb.DelayedSiacoinOutputDiffs = append(pb.DelayedSiacoinOutputDiffs, dscod)
		err := commitDelayedSiacoinOutputDiff(tx, dscod, modules.DiffApply)
		if err != nil {
			return err
		}
	}
	return nil
}

// applyMaturedSiacoinOutputs goes through the list of siacoin outputs that
// have matured and adds them to the consensus set. This also updates the block
// node diff set.
func applyMaturedSiacoinOutputs(tx *bolt.Tx, pb *processedBlock) error {
	// Skip this step if the blockchain is not old enough to have maturing
	// outputs.
	if !(pb.Height > types.MaturityDelay) {
		return nil
	}

	err := forEachDSCO(tx, pb.Height, func(id types.SiacoinOutputID, sco types.SiacoinOutput) error {
		// Sanity check - the output should not already be in siacoinOuptuts.
		if build.DEBUG && isSiacoinOutput(tx, id) {
			panic(errOutputAlreadyMature)
		}

		// Add the output to the ConsensusSet and record the diff in the
		// blockNode.
		scod := modules.SiacoinOutputDiff{
			Direction:     modules.DiffApply,
			ID:            id,
			SiacoinOutput: sco,
		}
		pb.SiacoinOutputDiffs = append(pb.SiacoinOutputDiffs, scod)
		err := commitSiacoinOutputDiff(tx, scod, modules.DiffApply)
		if err != nil {
			return err
		}

		// Remove the delayed siacoin output from the consensus set.
		dscod := modules.DelayedSiacoinOutputDiff{
			Direction:      modules.DiffRevert,
			ID:             id,
			SiacoinOutput:  sco,
			MaturityHeight: pb.Height,
		}
		pb.DelayedSiacoinOutputDiffs = append(pb.DelayedSiacoinOutputDiffs, dscod)
		return commitDelayedSiacoinOutputDiff(tx, dscod, modules.DiffApply)
	})
	if err != nil {
		return err
	}
	return deleteDSCOBucket(tx, pb.Height)
}

// applyMissedStorageProof adds the outputs and diffs that result from a file
// contract expiring.
func applyTxMissedStorageProof(tx *bolt.Tx, pb *processedBlock, fcid types.FileContractID) error {
	// Sanity checks.
	fc, err := getFileContract(tx, fcid)
	if err != nil {
		return err
	}
	if build.DEBUG {
		// Check that the file contract in question expires at pb.Height.
		if fc.WindowEnd != pb.Height {
			panic(errStorageProofTiming)
		}
	}

	// Add all of the outputs in the missed proof outputs to the consensus set.
	for i, mpo := range fc.MissedProofOutputs {
		// Sanity check - output should not already exist.
		spoid := fcid.StorageProofOutputID(types.ProofMissed, uint64(i))
		if build.DEBUG {
			exists := isSiacoinOutput(tx, spoid)
			if exists {
				panic(errPayoutsAlreadyPaid)
			}
		}

		dscod := modules.DelayedSiacoinOutputDiff{
			Direction:      modules.DiffApply,
			ID:             spoid,
			SiacoinOutput:  mpo,
			MaturityHeight: pb.Height + types.MaturityDelay,
		}
		pb.DelayedSiacoinOutputDiffs = append(pb.DelayedSiacoinOutputDiffs, dscod)
		err = commitDelayedSiacoinOutputDiff(tx, dscod, modules.DiffApply)
		if err != nil {
			return err
		}
	}

	// Remove the file contract from the consensus set and record the diff in
	// the blockNode.
	fcd := modules.FileContractDiff{
		Direction:    modules.DiffRevert,
		ID:           fcid,
		FileContract: fc,
	}
	pb.FileContractDiffs = append(pb.FileContractDiffs, fcd)
	return commitFileContractDiff(tx, fcd, modules.DiffApply)
}

// applyFileContractMaintenance looks for all of the file contracts that have
// expired without an appropriate storage proof, and calls 'applyMissedProof'
// for the file contract.
func applyFileContractMaintenance(tx *bolt.Tx, pb *processedBlock) error {
	// Get the bucket pointing to all of the expiring file contracts.
	fceBucketID := append(prefix_fcex, encoding.Marshal(pb.Height)...)
	fceBucket := tx.Bucket(fceBucketID)
	if fceBucket == nil {
		return nil
	}
	err := fceBucket.ForEach(func(keyBytes, valBytes []byte) error {
		var id types.FileContractID
		copy(id[:], keyBytes)
		return applyTxMissedStorageProof(tx, pb, id)
	})
	if err != nil {
		return err
	}
	return nil
	// return tx.DeleteBucket(fceBucketID)
}

// applyMaintenance applies block-level alterations to the consensus set.
// Maintenance is applied after all of the transcations for the block have been
// applied.
func applyMaintenance(tx *bolt.Tx, pb *processedBlock) error {
	err := applyMinerPayouts(tx, pb)
	if err != nil {
		return err
	}
	err = applyMaturedSiacoinOutputs(tx, pb)
	if err != nil {
		return err
	}
	return applyFileContractMaintenance(tx, pb)
}
