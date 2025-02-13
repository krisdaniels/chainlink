package keeper

import (
	"fmt"
	"sync"

	"github.com/pkg/errors"

	"github.com/smartcontractkit/chainlink/core/chains/evm/log"
	"github.com/smartcontractkit/chainlink/core/services/keystore/keys/ethkey"
	"github.com/smartcontractkit/chainlink/core/utils"
)

func (rs *RegistrySynchronizer) processLogs() {
	wg := sync.WaitGroup{}
	wg.Add(7)
	go rs.handleSyncRegistryLog(wg.Done)
	go rs.handleUpkeepCanceledLogs(wg.Done)
	go rs.handleUpkeepRegisteredLogs(wg.Done)
	go rs.handleUpkeepReceivedLogs(wg.Done)
	go rs.handleUpkeepMigratedLogs(wg.Done)
	go rs.handleUpkeepPerformedLogs(wg.Done)
	go rs.handleUpkeepGasLimitSetLogs(wg.Done)
	wg.Wait()
}

func (rs *RegistrySynchronizer) handleSyncRegistryLog(done func()) {
	defer done()
	broadcast, exists := rs.mailRoom.mbSyncRegistry.Retrieve()
	if !exists {
		return
	}
	txHash := broadcast.RawLog().TxHash.Hex()
	rs.logger.Debugw("processing SyncRegistry log", "txHash", txHash)
	was, err := rs.logBroadcaster.WasAlreadyConsumed(broadcast)
	if err != nil {
		rs.logger.Warn(errors.Wrap(err, "unable to check if log was consumed"))
		return
	}
	if was {
		return
	}
	_, err = rs.syncRegistry()
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to sync registry"))
		return
	}
	if err := rs.logBroadcaster.MarkConsumed(broadcast); err != nil {
		rs.logger.Error(errors.Wrapf(err, "unable to mark SyncRegistryLog log as consumed, log: %v", broadcast.String()))
	}
}

func (rs *RegistrySynchronizer) handleUpkeepCanceledLogs(done func()) {
	defer done()
	for {
		broadcast, exists := rs.mailRoom.mbUpkeepCanceled.Retrieve()
		if !exists {
			return
		}
		rs.handleUpkeepCancelled(broadcast)
	}
}

func (rs *RegistrySynchronizer) handleUpkeepCancelled(broadcast log.Broadcast) {
	txHash := broadcast.RawLog().TxHash.Hex()
	rs.logger.Debugw("processing UpkeepCanceled log", "txHash", txHash)
	was, err := rs.logBroadcaster.WasAlreadyConsumed(broadcast)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to check if log was consumed"))
		return
	}
	if was {
		return
	}

	cancelledID, err := rs.registryWrapper.GetCancelledUpkeepIDFromLog(broadcast)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "Unable to fetch cancelled upkeep ID from log"))
		return
	}

	affected, err := rs.orm.BatchDeleteUpkeepsForJob(rs.job.ID, []utils.Big{*utils.NewBig(cancelledID)})
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to batch delete upkeeps"))
		return
	}
	rs.logger.Debugw(fmt.Sprintf("deleted %v upkeep registrations", affected), "txHash", txHash)

	if err := rs.logBroadcaster.MarkConsumed(broadcast); err != nil {
		rs.logger.Error(errors.Wrapf(err, "unable to mark KeeperRegistryUpkeepCanceled log as consumed,  log: %v", broadcast.String()))
	}
}

func (rs *RegistrySynchronizer) handleUpkeepRegisteredLogs(done func()) {
	defer done()
	registry, err := rs.orm.RegistryForJob(rs.job.ID)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to find registry for job"))
		return
	}
	for {
		broadcast, exists := rs.mailRoom.mbUpkeepRegistered.Retrieve()
		if !exists {
			return
		}
		rs.HandleUpkeepRegistered(broadcast, registry)
	}
}

func (rs *RegistrySynchronizer) HandleUpkeepRegistered(broadcast log.Broadcast, registry Registry) {
	txHash := broadcast.RawLog().TxHash.Hex()
	rs.logger.Debugw("processing UpkeepRegistered log", "txHash", txHash)
	was, err := rs.logBroadcaster.WasAlreadyConsumed(broadcast)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to check if log was consumed"))
		return
	}
	if was {
		return
	}

	upkeepID, err := rs.registryWrapper.GetUpkeepIdFromRegistrationLog(broadcast)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "Unable to fetch upkeep ID from registration log"))
		return
	}

	err = rs.syncUpkeep(&rs.registryWrapper, registry, utils.NewBig(upkeepID))
	if err != nil {
		rs.logger.Error(errors.Wrapf(err, "failed to sync upkeep, log: %v", broadcast.String()))
		return
	}
	if err := rs.logBroadcaster.MarkConsumed(broadcast); err != nil {
		rs.logger.Error(errors.Wrapf(err, "unable to mark KeeperRegistryUpkeepRegistered log as consumed, log: %v", broadcast.String()))
	}
}

func (rs *RegistrySynchronizer) handleUpkeepPerformedLogs(done func()) {
	defer done()
	for {
		broadcast, exists := rs.mailRoom.mbUpkeepPerformed.Retrieve()
		if !exists {
			return
		}
		rs.handleUpkeepPerformed(broadcast)
	}
}

func (rs *RegistrySynchronizer) handleUpkeepPerformed(broadcast log.Broadcast) {
	txHash := broadcast.RawLog().TxHash.Hex()
	rs.logger.Debugw("processing UpkeepPerformed log", "jobID", rs.job.ID, "txHash", txHash)
	was, err := rs.logBroadcaster.WasAlreadyConsumed(broadcast)
	if err != nil {
		rs.logger.Warn(errors.Wrap(err, "unable to check if log was consumed"))
		return
	}

	if was {
		return
	}

	log, err := rs.registryWrapper.ParseUpkeepPerformedLog(broadcast)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "Unable to fetch upkeep ID from performed log"))
		return
	}
	rowsAffected, err := rs.orm.SetLastRunInfoForUpkeepOnJob(rs.job.ID, utils.NewBig(log.UpkeepID), int64(broadcast.RawLog().BlockNumber), ethkey.EIP55AddressFromAddress(log.FromKeeper))

	if err != nil {
		rs.logger.Error(errors.Wrap(err, "failed to set last run to 0"))
		return
	}
	rs.logger.Debugw("updated db for UpkeepPerformed log",
		"jobID", rs.job.ID,
		"upkeepID", log.UpkeepID.String(),
		"blockNumber", int64(broadcast.RawLog().BlockNumber),
		"fromAddr", ethkey.EIP55AddressFromAddress(log.FromKeeper),
		"rowsAffected", rowsAffected)

	if err := rs.logBroadcaster.MarkConsumed(broadcast); err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to mark KeeperRegistryUpkeepPerformed log as consumed"))
	}
}

func (rs *RegistrySynchronizer) handleUpkeepGasLimitSetLogs(done func()) {
	defer done()
	registry, err := rs.orm.RegistryForJob(rs.job.ID)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to find registry for job"))
		return
	}
	for {
		broadcast, exists := rs.mailRoom.mbUpkeepGasLimitSet.Retrieve()
		if !exists {
			return
		}
		rs.handleUpkeepGasLimitSet(broadcast, registry)
	}
}

func (rs *RegistrySynchronizer) handleUpkeepGasLimitSet(broadcast log.Broadcast, registry Registry) {
	txHash := broadcast.RawLog().TxHash.Hex()
	rs.logger.Debugw("processing UpkeepGasLimitSet log", "jobID", rs.job.ID, "txHash", txHash)
	was, err := rs.logBroadcaster.WasAlreadyConsumed(broadcast)
	if err != nil {
		rs.logger.Warn(errors.Wrap(err, "unable to check if log was consumed"))
		return
	}
	if was {
		return
	}

	upkeepID, err := rs.registryWrapper.GetIDFromGasLimitSetLog(broadcast)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "Unable to fetch upkeep ID from gas limit set log"))
		return
	}

	err = rs.syncUpkeep(&rs.registryWrapper, registry, utils.NewBig(upkeepID))
	if err != nil {
		rs.logger.Error(errors.Wrapf(err, "failed to sync upkeep, log: %v", broadcast.String()))
		return
	}
	if err := rs.logBroadcaster.MarkConsumed(broadcast); err != nil {
		rs.logger.Error(errors.Wrapf(err, "unable to mark KeeperRegistryUpkeepGasLimitSet log as consumed, log: %v", broadcast.String()))
	}
}

func (rs *RegistrySynchronizer) handleUpkeepReceivedLogs(done func()) {
	defer done()
	registry, err := rs.orm.RegistryForJob(rs.job.ID)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to find registry for job"))
		return
	}
	for {
		broadcast, exists := rs.mailRoom.mbUpkeepReceived.Retrieve()
		if !exists {
			return
		}
		rs.HandleUpkeepReceived(broadcast, registry)
	}
}

func (rs *RegistrySynchronizer) HandleUpkeepReceived(broadcast log.Broadcast, registry Registry) {
	txHash := broadcast.RawLog().TxHash.Hex()
	rs.logger.Debugw("processing UpkeepReceived log", "txHash", txHash)
	was, err := rs.logBroadcaster.WasAlreadyConsumed(broadcast)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to check if log was consumed"))
		return
	}
	if was {
		return
	}

	upkeepID, err := rs.registryWrapper.GetUpkeepIdFromReceivedLog(broadcast)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "Unable to fetch upkeep ID from received log"))
		return
	}

	err = rs.syncUpkeep(&rs.registryWrapper, registry, utils.NewBig(upkeepID))
	if err != nil {
		rs.logger.Error(errors.Wrapf(err, "failed to sync upkeep, log: %v", broadcast.String()))
		return
	}
	if err := rs.logBroadcaster.MarkConsumed(broadcast); err != nil {
		rs.logger.Error(errors.Wrapf(err, "unable to mark KeeperRegistryUpkeepReceived log as consumed, log: %v", broadcast.String()))
	}
}

func (rs *RegistrySynchronizer) handleUpkeepMigratedLogs(done func()) {
	defer done()
	for {
		broadcast, exists := rs.mailRoom.mbUpkeepMigrated.Retrieve()
		if !exists {
			return
		}
		rs.handleUpkeepMigrated(broadcast)
	}
}

func (rs *RegistrySynchronizer) handleUpkeepMigrated(broadcast log.Broadcast) {
	txHash := broadcast.RawLog().TxHash.Hex()
	rs.logger.Debugw("processing UpkeepMigrated log", "txHash", txHash)
	was, err := rs.logBroadcaster.WasAlreadyConsumed(broadcast)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to check if log was consumed"))
		return
	}
	if was {
		return
	}

	migratedID, err := rs.registryWrapper.GetUpkeepIdFromMigratedLog(broadcast)
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "Unable to fetch migrated upkeep ID from log"))
		return
	}

	affected, err := rs.orm.BatchDeleteUpkeepsForJob(rs.job.ID, []utils.Big{*utils.NewBig(migratedID)})
	if err != nil {
		rs.logger.Error(errors.Wrap(err, "unable to batch delete upkeeps"))
		return
	}
	rs.logger.Debugw(fmt.Sprintf("deleted %v upkeep registrations", affected), "txHash", txHash)

	if err := rs.logBroadcaster.MarkConsumed(broadcast); err != nil {
		rs.logger.Error(errors.Wrapf(err, "unable to mark KeeperRegistryUpkeepMigrated log as consumed,  log: %v", broadcast.String()))
	}
}
