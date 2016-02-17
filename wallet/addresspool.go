/*
 * Copyright (c) 2013-2015 The btcsuite developers
 * Copyright (c) 2015 The Decred developers
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package wallet

import (
	"fmt"
	"sync"

	"github.com/decred/dcrutil"
	"github.com/decred/dcrwallet/waddrmgr"
)

// addressPoolBuffer is the number of addresses to fetch when the address pool
// runs out of new addresses to use.
const addressPoolBuffer = 20

// addressPool is a cache of addresses to use that are generated by the
// address manager. It is safe than directly calling the address manager
// because doing that will increment the cursor of the extended key even
// if the created transaction errors out in some way.
type addressPool struct {
	// Represent addresses as strings because the address interface
	// doesn't have any good way to make comparisons.
	addresses []string
	cursor    int
	branch    uint32
	index     uint32
	started   bool
	mutex     *sync.Mutex
	wallet    *Wallet
}

// NewAddressPool creates a new address pool for the wallet default account.
func NewAddressPool() *addressPool {
	return &addressPool{
		started: false,
	}
}

// getLastAddressIndex retrieves the last known address index for the wallet
// default account's passed branch. If the address couldn't be found, it is
// assumed that the wallet is being newly initialized and 0, nil are returned.
func getLastAddressIndex(w *Wallet, branch uint32) (uint32, error) {
	var lastIndex uint32
	var err error
	var lastAddrFunc func(uint32) (waddrmgr.ManagedAddress, uint32, error)
	switch branch {
	case waddrmgr.InternalBranch:
		lastAddrFunc = w.Manager.LastInternalAddress
	case waddrmgr.ExternalBranch:
		lastAddrFunc = w.Manager.LastExternalAddress
	}

	if lastAddrFunc == nil {
		return 0, fmt.Errorf("unknown branch for last address index in address " +
			"pool")
	}

	_, lastIndex, err = lastAddrFunc(waddrmgr.DefaultAccountNum)
	if err != nil {
		if errMgr, ok := err.(waddrmgr.ManagerError); ok {
			if errMgr.ErrorCode == waddrmgr.ErrAddressNotFound {
				return 0, nil
			}
		}
		return 0, err
	}

	return lastIndex, nil
}

// initialize initializes an address pool for usage by loading the latest
// unused address from the blockchain itself.
func (a *addressPool) initialize(branch uint32, w *Wallet) error {
	// Do not reinitialize an address pool that was already started.
	// This can happen if the RPC client dies due to a disconnect
	// from the daemon.
	if a.started {
		return nil
	}

	a.addresses = make([]string, 0)
	a.mutex = new(sync.Mutex)
	a.wallet = w
	a.branch = branch

	var err error

	// Retrieve the next to use addresses from wallet closing and storing.
	lastExtAddr, lastIntAddr, err := w.Manager.NextToUseAddresses()
	if err != nil {
		return err
	}
	var lastSavedAddr dcrutil.Address
	switch branch {
	case waddrmgr.ExternalBranch:
		lastSavedAddr = lastExtAddr
	case waddrmgr.InternalBranch:
		lastSavedAddr = lastIntAddr
	default:
		return fmt.Errorf("unknown branch %v for wallet default account given",
			branch)
	}

	// Get the last managed address for the account and branch.
	lastIndex, err := getLastAddressIndex(w, branch)
	if lastIndex == 0 && err == nil {
		// Handle the case that the wallet is newly initialized.
		a.index = 0
		a.cursor = 0
		a.started = true
		return nil
	}

	// Get the actual last index as recorded in the blockchain.
	traversed := 0
	actualLastIndex := lastIndex
	for actualLastIndex != 0 && traversed != addressPoolBuffer {
		addr, err := a.wallet.Manager.GetAddress(actualLastIndex,
			waddrmgr.DefaultAccountNum, branch)
		if err != nil {
			return err
		}

		// Start with the address on tip if address reuse is disabled.
		if !w.addressReuse {
			// If address reuse is disabled, we compare to the last
			// stored address.
			if lastSavedAddr != nil {
				lsaH160 := lastSavedAddr.Hash160()
				thisH160 := addr.Hash160()
				if *lsaH160 == *thisH160 {
					// We actually append this address because the
					// LastUsedAddresses function in Manager actually
					// stores the next to-be-used address rather than
					// the last used address. See Close below.
					a.addresses = append([]string{addr.EncodeAddress()},
						a.addresses...)
					break
				}
			}
		} else {
			// Otherwise, search the blockchain for the last actually used
			// address.
			exists, err := a.wallet.existsAddressOnChain(addr)
			if err != nil {
				return err
			}
			if exists {
				break
			}
		}

		// Insert this unused address into the cache.
		a.addresses = append([]string{addr.EncodeAddress()},
			a.addresses...)

		actualLastIndex--
		traversed++
	}

	// DEBUG
	log.Infof("Last actual index on pool branch %v start: %v",
		branch, actualLastIndex)

	a.index = actualLastIndex
	a.cursor = 0
	a.started = true

	return nil
}

// GetNewAddress must be run as many times as necessary with the address pool
// mutex locked. Each time, it returns a single new address while adding that
// address to the toDelete map. If the address pool runs out of addresses, it
// generates more from the address manager.
func (a *addressPool) GetNewAddress() (dcrutil.Address, error) {
	if !a.started {
		return nil, fmt.Errorf("failed to GetNewAddress; pool not started")
	}

	// Replenish the pool if we're at the last address.
	if a.cursor == len(a.addresses)-1 || len(a.addresses) == 0 {
		var nextAddrFunc func(uint32, uint32) ([]waddrmgr.ManagedAddress, error)
		switch a.branch {
		case waddrmgr.InternalBranch:
			nextAddrFunc = a.wallet.Manager.NextInternalAddresses
		case waddrmgr.ExternalBranch:
			nextAddrFunc = a.wallet.Manager.NextExternalAddresses
		default:
			return nil, fmt.Errorf("unknown default account branch %v", a.branch)
		}

		addrs, err :=
			nextAddrFunc(waddrmgr.DefaultAccountNum, addressPoolBuffer)
		if err != nil {
			return nil, err
		}

		for _, addr := range addrs {
			a.addresses = append(a.addresses, addr.Address().EncodeAddress())
		}
	}

	// As these are all encoded addresses, we should never throw an error
	// converting back.
	curAddressStr := a.addresses[a.cursor]
	curAddress, _ := dcrutil.DecodeAddress(curAddressStr, a.wallet.chainParams)
	a.cursor++
	a.index++

	// DEBUG
	log.Infof("Get new address for branch %v returned %s (idx %v)",
		a.branch, curAddressStr, a.index)

	// Add the address to the notifications watcher.
	addrs := make([]dcrutil.Address, 1)
	addrs[0] = curAddress
	if err := a.wallet.chainSvr.NotifyReceived(addrs); err != nil {
		return nil, err
	}

	return curAddress, nil
}

// BatchFinish must be run after every successful series of usages of
// GetNewAddress to purge the addresses from the unused map.
func (a *addressPool) BatchFinish() {
	// We used all the addresses, so we need to pull new addresses
	// on the next call of this function.
	if a.cursor >= len(a.addresses) {
		a.addresses = nil
		a.cursor = 0
		return
	}

	// Write the next address to use to the database.
	addr, err := a.wallet.Manager.GetAddress(a.index+1,
		waddrmgr.DefaultAccountNum, a.branch)
	if err != nil {
		log.Errorf("Encountered unexpected error when trying to get "+
			"the next to use address for branch %v, index %v", a.branch,
			a.index+1)
	}
	switch a.branch {
	case waddrmgr.ExternalBranch:
		err = a.wallet.Manager.StoreNextToUseAddresses(addr, nil)
		if err != nil {
			log.Errorf("Failed to store next to use address for external "+
				"pool in the manager on batch finish: %v", err.Error())
		}
	case waddrmgr.InternalBranch:
		err = a.wallet.Manager.StoreNextToUseAddresses(nil, addr)
		if err != nil {
			log.Errorf("Failed to store next to use address for internal "+
				"pool in the manager on batch finish: %v", err.Error())
		}
	}

	a.addresses = a.addresses[a.cursor:len(a.addresses)]
	a.cursor = 0
}

// BatchRollback must be run after every unsuccessful series of usages
// of GetNewAddress to restore the cursor to the original position in
// the slice, thus marking all addresses unused again.
func (a *addressPool) BatchRollback() {
	a.index -= uint32(a.cursor)
	a.cursor = 0

	// DEBUG
	log.Infof("Batch rollback for branch %v to idx %v",
		a.branch, a.index)
}

// CloseAddressPools grabs one last new address for both internal and external
// acounts. Then it inserts them into the address manager database, so that
// the address manager can be used upon startup to restore the cursor position
// in the address pool.
func (w *Wallet) CloseAddressPools() {
	if w.internalPool == nil {
		return
	}
	if w.externalPool == nil {
		return
	}
	if !w.internalPool.started || !w.externalPool.started {
		return
	}
	if w.internalPool.mutex == nil {
		return
	}
	if w.externalPool.mutex == nil {
		return
	}

	w.internalPool.mutex.Lock()
	w.externalPool.mutex.Lock()
	defer w.internalPool.mutex.Unlock()
	defer w.externalPool.mutex.Unlock()

	nextExtAddr, err := w.externalPool.GetNewAddress()
	if err != nil {
		log.Errorf("Failed to get next to use address for address "+
			"pool external: %v", err.Error())
		return
	}
	nextIntAddr, err := w.internalPool.GetNewAddress()
	if err != nil {
		log.Errorf("Failed to get next to use address for address "+
			"pool internal: %v", err.Error())
		return
	}

	err = w.Manager.StoreNextToUseAddresses(nextExtAddr, nextIntAddr)
	if err != nil {
		log.Errorf("Failed to store next to use addresses for address "+
			"pools in the manager: %v", err.Error())
	}
	return
}

// GetNewAddressExternal is the exported function that gets a new external address
// for the default account from the external address mempool.
func (w *Wallet) GetNewAddressExternal() (dcrutil.Address, error) {
	w.externalPool.mutex.Lock()
	defer w.externalPool.mutex.Unlock()
	return w.externalPool.GetNewAddress()
}

// GetNewAddressExternal is the exported function that gets a new internal address
// for the default account from the internal address mempool.
func (w *Wallet) GetNewAddressInternal() (dcrutil.Address, error) {
	w.internalPool.mutex.Lock()
	defer w.internalPool.mutex.Unlock()
	return w.internalPool.GetNewAddress()
}
