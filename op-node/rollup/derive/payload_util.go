package derive

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum-optimism/optimism/op-core/forks"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// PayloadToBlockRef extracts the essential L2BlockRef information from an execution payload,
// falling back to genesis information if necessary.
func PayloadToBlockRef(rollupCfg *rollup.Config, payload *eth.ExecutionPayload) (eth.L2BlockRef, error) {
	genesis := &rollupCfg.Genesis
	var l1Origin eth.BlockID
	var sequenceNumber uint64
	if uint64(payload.BlockNumber) == genesis.L2.Number {
		if payload.BlockHash != genesis.L2.Hash {
			return eth.L2BlockRef{}, fmt.Errorf("expected L2 genesis hash to match L2 block at genesis block number %d: %s <> %s", genesis.L2.Number, payload.BlockHash, genesis.L2.Hash)
		}
		l1Origin = genesis.L1
		sequenceNumber = 0
	} else {
		if len(payload.Transactions) == 0 {
			return eth.L2BlockRef{}, fmt.Errorf("l2 block is missing L1 info deposit tx, block hash: %s", payload.BlockHash)
		}
		var tx types.Transaction
		if err := tx.UnmarshalBinary(payload.Transactions[0]); err != nil {
			return eth.L2BlockRef{}, fmt.Errorf("failed to decode first tx to read l1 info from: %w", err)
		}
		if tx.Type() != types.DepositTxType {
			return eth.L2BlockRef{}, fmt.Errorf("first payload tx has unexpected tx type: %d", tx.Type())
		}
		info, err := L1BlockInfoFromBytes(rollupCfg, uint64(payload.Timestamp), tx.Data())
		if err != nil {
			return eth.L2BlockRef{}, fmt.Errorf("failed to parse L1 info deposit tx from L2 block: %w", err)
		}
		l1Origin = eth.BlockID{Hash: info.BlockHash, Number: info.Number}
		sequenceNumber = info.SequenceNumber
	}

	return eth.L2BlockRef{
		Hash:           payload.BlockHash,
		Number:         uint64(payload.BlockNumber),
		ParentHash:     payload.ParentHash,
		Time:           uint64(payload.Timestamp),
		L1Origin:       l1Origin,
		SequenceNumber: sequenceNumber,
	}, nil
}

func PayloadToSystemConfig(rollupCfg *rollup.Config, payload *eth.ExecutionPayload) (eth.SystemConfig, error) {
	if uint64(payload.BlockNumber) == rollupCfg.Genesis.L2.Number {
		if payload.BlockHash != rollupCfg.Genesis.L2.Hash {
			return eth.SystemConfig{}, fmt.Errorf(
				"expected L2 genesis hash to match L2 block at genesis block number %d: %s <> %s",
				rollupCfg.Genesis.L2.Number, payload.BlockHash, rollupCfg.Genesis.L2.Hash)
		}
		return rollupCfg.Genesis.SystemConfig, nil
	}

	if len(payload.Transactions) == 0 {
		return eth.SystemConfig{}, fmt.Errorf("l2 block is missing L1 info deposit tx, block hash: %s", payload.BlockHash)
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(payload.Transactions[0]); err != nil {
		return eth.SystemConfig{}, fmt.Errorf("failed to decode first tx to read l1 info from: %w", err)
	}
	if tx.Type() != types.DepositTxType {
		return eth.SystemConfig{}, fmt.Errorf("first payload tx has unexpected tx type: %d", tx.Type())
	}
	info, err := L1BlockInfoFromBytes(rollupCfg, uint64(payload.Timestamp), tx.Data())
	if err != nil {
		return eth.SystemConfig{}, fmt.Errorf("failed to parse L1 info deposit tx from L2 block: %w", err)
	}
	if isEcotoneButNotFirstBlock(rollupCfg, uint64(payload.Timestamp)) {
		// Translate Ecotone values back into encoded scalar if needed.
		// We do not know if it was derived from a v0 or v1 scalar,
		// but v1 is fine, a 0 blob base fee has the same effect.
		info.L1FeeScalar[0] = 1
		binary.BigEndian.PutUint32(info.L1FeeScalar[24:28], info.BlobBaseFeeScalar)
		binary.BigEndian.PutUint32(info.L1FeeScalar[28:32], info.BaseFeeScalar)
	}
	r := eth.SystemConfig{
		BatcherAddr: info.BatcherAddr,
		Overhead:    info.L1FeeOverhead,
		Scalar:      info.L1FeeScalar,
		GasLimit:    uint64(payload.GasLimit),
	}

	// Starting with Karst, the activation block's gas limit carries one-time upgrade gas on
	// top of the system config gas limit so the NUT-bundle upgrade transactions don't have to
	// fit within it (see PreparePayloadAttributes). StripsKarstUpgradeGas subtracts it back out
	// at the block right after the activation block, unless KeepKarstUpgradeGas opts out. This
	// runs before UpdateSystemConfigWithL1Receipts in PreparePayloadAttributes, so a setGasLimit
	// in the same block's L1 origin takes precedence.
	if rollupCfg.StripsKarstUpgradeGas(uint64(payload.Timestamp)) {
		karstGas, err := UpgradeGas(forks.Karst)
		if err != nil {
			return eth.SystemConfig{}, fmt.Errorf("failed to read karst upgrade gas: %w", err)
		}
		if r.GasLimit < karstGas {
			return eth.SystemConfig{}, fmt.Errorf("karst activation block gas limit %d below upgrade gas %d", r.GasLimit, karstGas)
		}
		r.GasLimit -= karstGas
	}
	err = eip1559.ValidateOptimismExtraData(rollupCfg, uint64(payload.Timestamp), payload.ExtraData)
	if err != nil {
		return eth.SystemConfig{}, err
	}
	d, e, m := eip1559.DecodeOptimismExtraData(rollupCfg, uint64(payload.Timestamp), payload.ExtraData)
	copy(r.EIP1559Params[:], eip1559.EncodeHolocene1559Params(d, e))

	if rollupCfg.IsIsthmus(uint64(payload.Timestamp)) {
		r.OperatorFeeParams = eth.EncodeOperatorFeeParams(eth.OperatorFeeParams{
			Scalar:   info.OperatorFeeScalar,
			Constant: info.OperatorFeeConstant,
		})
	}

	if rollupCfg.IsJovian(uint64(payload.Timestamp)) {
		// ValidateOptimismExtraData returning a nil error guarantees that m is not nil
		r.MinBaseFee = *m
		r.DAFootprintGasScalar = info.DAFootprintGasScalar
	}
	return r, nil
}
