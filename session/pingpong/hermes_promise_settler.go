/*
 * Copyright (C) 2019 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package pingpong

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	nodevent "github.com/mysteriumnetwork/node/core/node/event"
	"github.com/mysteriumnetwork/node/core/service/servicestate"
	"github.com/mysteriumnetwork/node/eventbus"
	"github.com/mysteriumnetwork/node/identity"
	"github.com/mysteriumnetwork/node/identity/registry"
	"github.com/mysteriumnetwork/node/session/pingpong/event"
	"github.com/mysteriumnetwork/payments/bindings"
	"github.com/mysteriumnetwork/payments/client"
	"github.com/mysteriumnetwork/payments/crypto"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

type settlementHistoryStorage interface {
	Store(provider identity.Identity, hermes common.Address, she SettlementHistoryEntry) error
}

type providerChannelStatusProvider interface {
	SubscribeToPromiseSettledEvent(providerID, hermesID common.Address) (sink chan *bindings.HermesImplementationPromiseSettled, cancel func(), err error)
	GetProviderChannel(hermesAddress common.Address, addressToCheck common.Address, pending bool) (client.ProviderChannel, error)
	GetHermesFee(hermesAddress common.Address) (uint16, error)
}

type ks interface {
	Accounts() []accounts.Account
}

type registrationStatusProvider interface {
	GetRegistrationStatus(id identity.Identity) (registry.RegistrationStatus, error)
}

type transactor interface {
	FetchSettleFees() (registry.FeesResponse, error)
	SettleAndRebalance(hermesID, providerID string, promise crypto.Promise) error
	SettleWithBeneficiary(id, beneficiary, hermesID string, promise crypto.Promise) error
}

type promiseStorage interface {
	Get(id identity.Identity, hermesID common.Address) (HermesPromise, error)
}

type receivedPromise struct {
	provider identity.Identity
	promise  crypto.Promise
}

// HermesPromiseSettler is responsible for settling the hermes promises.
type HermesPromiseSettler interface {
	GetEarnings(id identity.Identity) event.Earnings
	ForceSettle(providerID identity.Identity, hermesID common.Address) error
	SettleWithBeneficiary(providerID identity.Identity, beneficiary, hermesID common.Address) error
	Subscribe() error
	GetHermesFee() (uint16, error)
}

// hermesPromiseSettler is responsible for settling the hermes promises.
type hermesPromiseSettler struct {
	eventBus                   eventbus.EventBus
	bc                         providerChannelStatusProvider
	config                     HermesPromiseSettlerConfig
	lock                       sync.RWMutex
	registrationStatusProvider registrationStatusProvider
	ks                         ks
	transactor                 transactor
	promiseStorage             promiseStorage
	settlementHistoryStorage   settlementHistoryStorage

	currentState map[identity.Identity]settlementState
	settleQueue  chan receivedPromise
	stop         chan struct{}
	once         sync.Once
}

// HermesPromiseSettlerConfig configures the hermes promise settler accordingly.
type HermesPromiseSettlerConfig struct {
	HermesAddress        common.Address
	Threshold            float64
	MaxWaitForSettlement time.Duration
}

// NewHermesPromiseSettler creates a new instance of hermes promise settler.
func NewHermesPromiseSettler(eventBus eventbus.EventBus, transactor transactor, promiseStorage promiseStorage, providerChannelStatusProvider providerChannelStatusProvider, registrationStatusProvider registrationStatusProvider, ks ks, settlementHistoryStorage settlementHistoryStorage, config HermesPromiseSettlerConfig) *hermesPromiseSettler {
	return &hermesPromiseSettler{
		eventBus:                   eventBus,
		bc:                         providerChannelStatusProvider,
		ks:                         ks,
		registrationStatusProvider: registrationStatusProvider,
		config:                     config,
		currentState:               make(map[identity.Identity]settlementState),
		promiseStorage:             promiseStorage,
		settlementHistoryStorage:   settlementHistoryStorage,

		// defaulting to a queue of 5, in case we have a few active identities.
		settleQueue: make(chan receivedPromise, 5),
		stop:        make(chan struct{}),
		transactor:  transactor,
	}
}

// GetHermesFee fetches the hermes fee.
func (aps *hermesPromiseSettler) GetHermesFee() (uint16, error) {
	return aps.bc.GetHermesFee(aps.config.HermesAddress)
}

// loadInitialState loads the initial state for the given identity. Inteded to be called on service start.
func (aps *hermesPromiseSettler) loadInitialState(addr identity.Identity) error {
	aps.lock.Lock()
	defer aps.lock.Unlock()

	if _, ok := aps.currentState[addr]; ok {
		log.Info().Msgf("State for %v already loaded, skipping", addr)
		return nil
	}

	status, err := aps.registrationStatusProvider.GetRegistrationStatus(addr)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("could not check registration status for %v", addr))
	}

	if status != registry.Registered {
		log.Info().Msgf("Provider %v not registered, skipping", addr)
		return nil
	}

	return aps.resyncState(addr)
}

func (aps *hermesPromiseSettler) resyncState(id identity.Identity) error {
	channel, err := aps.bc.GetProviderChannel(aps.config.HermesAddress, id.ToCommonAddress(), true)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("could not get provider channel for %v", id))
	}

	hermesPromise, err := aps.promiseStorage.Get(id, aps.config.HermesAddress)
	if err != nil && err != ErrNotFound {
		return errors.Wrap(err, fmt.Sprintf("could not get hermes promise for %v", id))
	}

	s := settlementState{
		channel:     channel,
		lastPromise: hermesPromise.Promise,
		registered:  true,
	}

	go aps.publishChangeEvent(id, aps.currentState[id], s)
	aps.currentState[id] = s
	log.Info().Msgf("Loaded state for provider %q: balance %v, available balance %v, unsettled balance %v", id, s.balance(), s.availableBalance(), s.unsettledBalance())
	return nil
}

func (aps *hermesPromiseSettler) publishChangeEvent(id identity.Identity, before, after settlementState) {
	aps.eventBus.Publish(event.AppTopicEarningsChanged, event.AppEventEarningsChanged{
		Identity: id,
		Previous: before.Earnings(),
		Current:  after.Earnings(),
	})
}

// Subscribe subscribes the hermes promise settler to the appropriate events
func (aps *hermesPromiseSettler) Subscribe() error {
	err := aps.eventBus.SubscribeAsync(nodevent.AppTopicNode, aps.handleNodeEvent)
	if err != nil {
		return errors.Wrap(err, "could not subscribe to node status event")
	}

	err = aps.eventBus.SubscribeAsync(registry.AppTopicIdentityRegistration, aps.handleRegistrationEvent)
	if err != nil {
		return errors.Wrap(err, "could not subscribe to registration event")
	}

	err = aps.eventBus.SubscribeAsync(servicestate.AppTopicServiceStatus, aps.handleServiceEvent)
	if err != nil {
		return errors.Wrap(err, "could not subscribe to service status event")
	}

	err = aps.eventBus.SubscribeAsync(event.AppTopicSettlementRequest, aps.handleSettlementEvent)
	if err != nil {
		return errors.Wrap(err, "could not subscribe to settlement event")
	}

	err = aps.eventBus.SubscribeAsync(event.AppTopicHermesPromise, aps.handleHermesPromiseReceived)
	return errors.Wrap(err, "could not subscribe to hermes promise event")
}

func (aps *hermesPromiseSettler) handleSettlementEvent(event event.AppEventSettlementRequest) {
	err := aps.ForceSettle(event.ProviderID, event.HermesID)
	if err != nil {
		log.Error().Err(err).Msg("could not settle promise")
	}
}

func (aps *hermesPromiseSettler) handleServiceEvent(event servicestate.AppEventServiceStatus) {
	switch event.Status {
	case string(servicestate.Running):
		err := aps.loadInitialState(identity.FromAddress(event.ProviderID))
		if err != nil {
			log.Error().Err(err).Msgf("could not load initial state for provider %v", event.ProviderID)
		}
	default:
		log.Debug().Msgf("Ignoring service event with status %v", event.Status)
	}
}

func (aps *hermesPromiseSettler) handleNodeEvent(payload nodevent.Payload) {
	if payload.Status == nodevent.StatusStarted {
		aps.handleNodeStart()
		return
	}

	if payload.Status == nodevent.StatusStopped {
		aps.handleNodeStop()
		return
	}
}

func (aps *hermesPromiseSettler) handleRegistrationEvent(payload registry.AppEventIdentityRegistration) {
	aps.lock.Lock()
	defer aps.lock.Unlock()

	if payload.Status != registry.Registered {
		log.Debug().Msgf("Ignoring event %v for provider %q", payload.Status.String(), payload.ID)
		return
	}
	log.Info().Msgf("Identity registration event received for provider %q", payload.ID)

	err := aps.resyncState(payload.ID)
	if err != nil {
		log.Error().Err(err).Msgf("Could not resync state for provider %v", payload.ID)
		return
	}

	log.Info().Msgf("Identity registration event handled for provider %q", payload.ID)
}

func (aps *hermesPromiseSettler) handleHermesPromiseReceived(apep event.AppEventHermesPromise) {
	id := apep.ProviderID
	log.Info().Msgf("Received hermes promise for %q", id)
	aps.lock.Lock()
	defer aps.lock.Unlock()

	s, ok := aps.currentState[apep.ProviderID]
	if !ok {
		log.Error().Msgf("Have no info on provider %q, skipping", id)
		return
	}
	if !s.registered {
		log.Error().Msgf("provider %q not registered, skipping", id)
		return
	}
	s.lastPromise = apep.Promise

	go aps.publishChangeEvent(id, aps.currentState[id], s)
	aps.currentState[apep.ProviderID] = s
	log.Info().Msgf("Hermes promise state updated for provider %q", id)

	if s.needsSettling(aps.config.Threshold) {
		aps.initiateSettling(apep.ProviderID, apep.HermesID)
	}
}

func (aps *hermesPromiseSettler) initiateSettling(providerID identity.Identity, hermesID common.Address) {
	promise, err := aps.promiseStorage.Get(providerID, hermesID)
	if err == ErrNotFound {
		log.Debug().Msgf("no promise to settle for %q %q", providerID, hermesID.Hex())
		return
	}
	if err != nil {
		log.Error().Err(fmt.Errorf("could not get promise from storage: %w", err))
		return
	}

	hexR, err := hex.DecodeString(promise.R)
	if err != nil {
		log.Error().Err(fmt.Errorf("could encode R: %w", err))
		return
	}
	promise.Promise.R = hexR

	aps.settleQueue <- receivedPromise{
		provider: providerID,
		promise:  promise.Promise,
	}
}

func (aps *hermesPromiseSettler) listenForSettlementRequests() {
	log.Info().Msg("Listening for settlement events")
	defer func() {
		log.Info().Msg("Stopped listening for settlement events")
	}()

	for {
		select {
		case <-aps.stop:
			return
		case p := <-aps.settleQueue:
			go aps.settle(p, nil)
		}
	}
}

// GetEarnings returns current settlement status for given identity
func (aps *hermesPromiseSettler) GetEarnings(id identity.Identity) event.Earnings {
	aps.lock.RLock()
	defer aps.lock.RUnlock()

	return aps.currentState[id].Earnings()
}

// ErrNothingToSettle indicates that there is nothing to settle.
var ErrNothingToSettle = errors.New("nothing to settle for the given provider")

// ForceSettle forces the settlement for a provider
func (aps *hermesPromiseSettler) ForceSettle(providerID identity.Identity, hermesID common.Address) error {
	promise, err := aps.promiseStorage.Get(providerID, hermesID)
	if err == ErrNotFound {
		return ErrNothingToSettle
	}
	if err != nil {
		return errors.Wrap(err, "could not get promise from storage")
	}

	hexR, err := hex.DecodeString(promise.R)
	if err != nil {
		return errors.Wrap(err, "could not decode R")
	}

	promise.Promise.R = hexR
	return aps.settle(receivedPromise{
		promise:  promise.Promise,
		provider: providerID,
	}, nil)
}

// ForceSettle forces the settlement for a provider
func (aps *hermesPromiseSettler) SettleWithBeneficiary(providerID identity.Identity, beneficiary, hermesID common.Address) error {
	promise, err := aps.promiseStorage.Get(providerID, hermesID)
	fmt.Println(promise, err)
	if err == ErrNotFound {
		return ErrNothingToSettle
	}
	if err != nil {
		return errors.Wrap(err, "could not get promise from storage")
	}

	hexR, err := hex.DecodeString(promise.R)
	if err != nil {
		return errors.Wrap(err, "could not decode R")
	}

	promise.Promise.R = hexR
	return aps.settle(receivedPromise{
		promise:  promise.Promise,
		provider: providerID,
	}, &beneficiary)
}

// ErrSettleTimeout indicates that the settlement has timed out
var ErrSettleTimeout = errors.New("settle timeout")

func (aps *hermesPromiseSettler) settle(p receivedPromise, beneficiary *common.Address) error {
	if aps.isSettling(p.provider) {
		return errors.New("provider already has settlement in progress")
	}

	aps.setSettling(p.provider, true)
	log.Info().Msgf("Marked provider %v as requesting settlement", p.provider)
	sink, cancel, err := aps.bc.SubscribeToPromiseSettledEvent(p.provider.ToCommonAddress(), aps.config.HermesAddress)
	if err != nil {
		aps.setSettling(p.provider, false)
		log.Error().Err(err).Msg("Could not subscribe to promise settlement")
		return err
	}

	errCh := make(chan error)
	go func() {
		defer cancel()
		defer aps.setSettling(p.provider, false)
		defer close(errCh)
		select {
		case <-aps.stop:
			return
		case info, more := <-sink:
			if !more || info == nil {
				break
			}

			log.Info().Msgf("Settling complete for provider %v", p.provider)

			she := SettlementHistoryEntry{
				TxHash:       info.Raw.TxHash,
				Promise:      p.promise,
				Amount:       info.Amount,
				TotalSettled: info.TotalSettled,
			}
			if beneficiary != nil {
				she.Beneficiary = *beneficiary
			}

			err := aps.settlementHistoryStorage.Store(p.provider, aps.config.HermesAddress, she)
			if err != nil {
				log.Error().Err(err).Msgf("could not store settlement history")
			}

			err = aps.resyncState(p.provider)
			if err != nil {
				// This will get retried so we do not need to explicitly retry
				// TODO: maybe add a sane limit of retries
				log.Error().Err(err).Msgf("Resync failed for provider %v", p.provider)
			} else {
				log.Info().Msgf("Resync success for provider %v", p.provider)
			}
			return
		case <-time.After(aps.config.MaxWaitForSettlement):
			log.Info().Msgf("Settle timeout for %v", p.provider)

			// send a signal to waiter that the settlement has timed out
			errCh <- ErrSettleTimeout
			return
		}
	}()

	var settleFunc = func() error {
		return aps.transactor.SettleAndRebalance(aps.config.HermesAddress.Hex(), p.provider.Address, p.promise)
	}
	if beneficiary != nil {
		settleFunc = func() error {
			return aps.transactor.SettleWithBeneficiary(p.provider.Address, beneficiary.Hex(), aps.config.HermesAddress.Hex(), p.promise)
		}
	}

	err = settleFunc()
	if err != nil {
		cancel()
		log.Error().Err(err).Msgf("Could not settle promise for %v", p.provider.Address)
		return err
	}

	return <-errCh
}

func (aps *hermesPromiseSettler) isSettling(id identity.Identity) bool {
	aps.lock.RLock()
	defer aps.lock.RUnlock()
	v, ok := aps.currentState[id]
	if !ok {
		return false
	}

	return v.settleInProgress
}

func (aps *hermesPromiseSettler) setSettling(id identity.Identity, settling bool) {
	aps.lock.Lock()
	defer aps.lock.Unlock()
	v := aps.currentState[id]
	v.settleInProgress = settling
	aps.currentState[id] = v
}

func (aps *hermesPromiseSettler) handleNodeStart() {
	go aps.listenForSettlementRequests()

	for _, v := range aps.ks.Accounts() {
		addr := identity.FromAddress(v.Address.Hex())
		go func(address identity.Identity) {
			err := aps.loadInitialState(address)
			if err != nil {
				log.Error().Err(err).Msgf("could not load initial state for %v", addr)
			}
		}(addr)
	}
}

func (aps *hermesPromiseSettler) handleNodeStop() {
	aps.once.Do(func() {
		close(aps.stop)
	})
}

// settlementState earning calculations model
type settlementState struct {
	channel     client.ProviderChannel
	lastPromise crypto.Promise

	settleInProgress bool
	registered       bool
}

// lifetimeBalance returns earnings of all history.
func (ss settlementState) lifetimeBalance() uint64 {
	return ss.lastPromise.Amount
}

// unsettledBalance returns current unsettled earnings.
func (ss settlementState) unsettledBalance() uint64 {
	settled := uint64(0)
	if ss.channel.Settled != nil {
		settled = ss.channel.Settled.Uint64()
	}

	return safeSub(ss.lastPromise.Amount, settled)
}

func (ss settlementState) availableBalance() uint64 {
	balance := uint64(0)
	if ss.channel.Balance != nil {
		balance = ss.channel.Balance.Uint64()
	}

	settled := uint64(0)
	if ss.channel.Settled != nil {
		settled = ss.channel.Settled.Uint64()
	}

	return balance + settled
}

func (ss settlementState) balance() uint64 {
	return safeSub(ss.availableBalance(), ss.lastPromise.Amount)
}

func (ss settlementState) needsSettling(threshold float64) bool {
	if !ss.registered {
		return false
	}

	if ss.settleInProgress {
		return false
	}

	calculatedThreshold := threshold * float64(ss.availableBalance())
	possibleEarnings := float64(ss.unsettledBalance())
	if possibleEarnings < calculatedThreshold {
		return false
	}

	if float64(ss.balance()) < calculatedThreshold {
		return true
	}

	return false
}

func (ss settlementState) Earnings() event.Earnings {
	return event.Earnings{
		LifetimeBalance:  ss.lifetimeBalance(),
		UnsettledBalance: ss.unsettledBalance(),
	}
}