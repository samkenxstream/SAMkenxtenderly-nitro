// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package precompiles

import (
	"errors"
	"math/big"

	"github.com/tenderly/nitro/go-ethereum/common"
)

// This precompile provides owners with tools for managing the rollup.
// All calls to this precompile are authorized by the OwnerPrecompile wrapper,
// which ensures only a chain owner can access these methods. For methods that
// are safe for non-owners to call, see ArbOwnerOld
type ArbOwner struct {
	Address          addr // 0x70
	OwnerActs        func(ctx, mech, bytes4, addr, []byte) error
	OwnerActsGasCost func(bytes4, addr, []byte) (uint64, error)
}

var (
	ErrOutOfBounds = errors.New("value out of bounds")
)

// Add account as a chain owner
func (con ArbOwner) AddChainOwner(c ctx, evm mech, newOwner addr) error {
	return c.State.ChainOwners().Add(newOwner)
}

// Remove account from the list of chain owners
func (con ArbOwner) RemoveChainOwner(c ctx, evm mech, addr addr) error {
	member, _ := con.IsChainOwner(c, evm, addr)
	if !member {
		return errors.New("tried to remove non-owner")
	}
	return c.State.ChainOwners().Remove(addr)
}

// See if the account is a chain owner
func (con ArbOwner) IsChainOwner(c ctx, evm mech, addr addr) (bool, error) {
	return c.State.ChainOwners().IsMember(addr)
}

// Retrieves the list of chain owners
func (con ArbOwner) GetAllChainOwners(c ctx, evm mech) ([]common.Address, error) {
	return c.State.ChainOwners().AllMembers(65536)
}

// Set how slowly ArbOS updates its estimate of the L1 basefee
func (con ArbOwner) SetL1BaseFeeEstimateInertia(c ctx, evm mech, inertia uint64) error {
	return c.State.L1PricingState().SetInertia(inertia)
}

// Sets the L2 gas price directly, bypassing the pool calculus
func (con ArbOwner) SetL2BaseFee(c ctx, evm mech, priceInWei huge) error {
	return c.State.L2PricingState().SetBaseFeeWei(priceInWei)
}

// Sets the minimum base fee needed for a transaction to succeed
func (con ArbOwner) SetMinimumL2BaseFee(c ctx, evm mech, priceInWei huge) error {
	return c.State.L2PricingState().SetMinBaseFeeWei(priceInWei)
}

// Sets the computational speed limit for the chain
func (con ArbOwner) SetSpeedLimit(c ctx, evm mech, limit uint64) error {
	return c.State.L2PricingState().SetSpeedLimitPerSecond(limit)
}

// Sets the maximum size a tx (and block) can be
func (con ArbOwner) SetMaxTxGasLimit(c ctx, evm mech, limit uint64) error {
	return c.State.L2PricingState().SetMaxPerBlockGasLimit(limit)
}

// Set the L2 gas pricing inertia
func (con ArbOwner) SetL2GasPricingInertia(c ctx, evm mech, sec uint64) error {
	return c.State.L2PricingState().SetPricingInertia(sec)
}

// Set the L2 gas backlog tolerance
func (con ArbOwner) SetL2GasBacklogTolerance(c ctx, evm mech, sec uint64) error {
	return c.State.L2PricingState().SetBacklogTolerance(sec)
}

// Gets the network fee collector
func (con ArbOwner) GetNetworkFeeAccount(c ctx, evm mech) (addr, error) {
	return c.State.NetworkFeeAccount()
}

// Sets the network fee collector
func (con ArbOwner) SetNetworkFeeAccount(c ctx, evm mech, newNetworkFeeAccount addr) error {
	return c.State.SetNetworkFeeAccount(newNetworkFeeAccount)
}

// Upgrades ArbOS to the requested version at the requested timestamp
func (con ArbOwner) ScheduleArbOSUpgrade(c ctx, evm mech, newVersion uint64, timestamp uint64) error {
	return c.State.ScheduleArbOSUpgrade(newVersion, timestamp)
}

func (con ArbOwner) SetL1PricingEquilibrationUnits(c ctx, evm mech, equilibrationUnits huge) error {
	return c.State.L1PricingState().SetEquilibrationUnits(equilibrationUnits)
}

func (con ArbOwner) SetL1PricingInertia(c ctx, evm mech, inertia uint64) error {
	return c.State.L1PricingState().SetInertia(inertia)
}

func (con ArbOwner) SetL1PricingRewardRecipient(c ctx, evm mech, recipient addr) error {
	return c.State.L1PricingState().SetPayRewardsTo(recipient)
}

func (con ArbOwner) SetL1PricingRewardRate(c ctx, evm mech, weiPerUnit uint64) error {
	return c.State.L1PricingState().SetPerUnitReward(weiPerUnit)
}

func (con ArbOwner) SetL1PricePerUnit(c ctx, evm mech, pricePerUnit *big.Int) error {
	return c.State.L1PricingState().SetPricePerUnit(pricePerUnit)
}

func (con ArbOwner) SetPerBatchGasCharge(c ctx, evm mech, cost int64) error {
	return c.State.L1PricingState().SetPerBatchGasCost(cost)
}

func (con ArbOwner) SetAmortizedCostCapBips(c ctx, evm mech, cap uint64) error {
	return c.State.L1PricingState().SetAmortizedCostCapBips(cap)
}
