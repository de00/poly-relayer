/*
 * Copyright (C) 2021 The poly network Authors
 * This file is part of The poly network library.
 *
 * The  poly network  is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Lesser General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * The  poly network  is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Lesser General Public License for more details.
 * You should have received a copy of the GNU Lesser General Public License
 * along with The poly network .  If not, see <http://www.gnu.org/licenses/>.
 */

package poly

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/polynetwork/bridge-common/base"
	"github.com/polynetwork/bridge-common/chains"
	"github.com/polynetwork/bridge-common/chains/poly"
	"github.com/polynetwork/bridge-common/log"
	"github.com/polynetwork/bridge-common/util"
	"github.com/polynetwork/poly-relayer/config"
	"github.com/polynetwork/poly-relayer/msg"
)

type Listener struct {
	sdk    *poly.SDK
	config *config.ListenerConfig
}

func (l *Listener) Init(config *config.ListenerConfig, sdk *poly.SDK) (err error) {
	l.config = config
	if sdk != nil {
		l.sdk = sdk
	} else {
		l.sdk, err = poly.WithOptions(base.POLY, config.Nodes, time.Minute, 1)
	}
	return
}

func (l *Listener) ScanDst(height uint64) (txs []*msg.Tx, err error) {
	txs, err = l.Scan(height)
	if err != nil { return }
	sub := &Submitter{sdk:l.sdk}
	for _, tx := range txs {
		tx.MerkleValue, _, _, err = sub.GetProof(tx.PolyHeight, tx.PolyKey)
		if err != nil { return }
	}
	return
}

func (l *Listener) Scan(height uint64) (txs []*msg.Tx, err error) {
	events, err := l.sdk.Node().GetSmartContractEventByBlock(uint32(height))
	if err != nil {
		return nil, err
	}

	for _, event := range events {
		for _, notify := range event.Notify {
			if notify.ContractAddress == poly.CCM_ADDRESS {
				states := notify.States.([]interface{})
				if len(states) < 6 {
					continue
				}
				method, _ := states[0].(string)
				if method != "makeProof" {
					continue
				}

				dstChain := uint64(states[2].(float64))
				if dstChain == 0 {
					log.Error("Invalid dst chain id in poly tx", "hash", event.TxHash)
					continue
				}

				tx := new(msg.Tx)
				tx.DstChainId = dstChain
				tx.PolyKey = states[5].(string)
				tx.PolyHeight = uint32(height)
				tx.PolyHash = event.TxHash
				tx.TxType = msg.POLY
				tx.TxId = states[3].(string)
				tx.SrcChainId = uint64(states[1].(float64))
				switch tx.SrcChainId {
				case base.NEO, base.NEO3, base.ONT:
					tx.TxId = util.ReverseHex(tx.TxId)
				}
				txs = append(txs, tx)
			}
		}
	}

	return
}

func (l *Listener) GetTxBlock(hash string) (height uint64, err error) {
	h, err := l.sdk.Node().GetBlockHeightByTxHash(hash)
	height = uint64(h)
	return
}

func (l *Listener) ScanTx(hash string) (tx *msg.Tx, err error) {
	return l.scanTx(l.sdk.Node(), hash)
}

func (l *Listener) scanTx(node *poly.Client, hash string) (tx *msg.Tx, err error) {
	//hash hasn't '0x'
	event, err := node.GetSmartContractEvent(hash)
	if err != nil {
		return nil, err
	}
	for _, notify := range event.Notify {
		if notify.ContractAddress == poly.CCM_ADDRESS {
			states := notify.States.([]interface{})
			if len(states) < 6 {
				continue
			}
			method, _ := states[0].(string)
			if method != "makeProof" {
				continue
			}

			dstChain := uint64(states[2].(float64))
			if dstChain == 0 {
				log.Error("Invalid dst chain id in poly tx", "hash", event.TxHash)
				continue
			}

			tx := new(msg.Tx)
			tx.DstChainId = dstChain
			tx.PolyKey = states[5].(string)
			tx.PolyHeight = uint32(states[4].(float64))
			tx.PolyHash = event.TxHash
			tx.TxType = msg.POLY
			tx.TxId = states[3].(string)
			tx.SrcChainId = uint64(states[1].(float64))
			switch tx.SrcChainId {
			case base.NEO, base.NEO3, base.ONT:
				tx.TxId = util.ReverseHex(tx.TxId)
			}
			return tx, nil
		}
	}
	return nil, errors.New(fmt.Sprintf("hash:%v hasn't event", hash))
}

func (l *Listener) ChainId() uint64 {
	return base.POLY
}

func (l *Listener) Compose(tx *msg.Tx) (err error) {
	return
}

func (l *Listener) Defer() int {
	return 1
}

func (l *Listener) Header(uint64) (header []byte, hash []byte, err error) {
	return
}

func (l *Listener) ListenCheck() time.Duration {
	duration := time.Second
	if l.config.ListenCheck > 0 {
		duration = time.Duration(l.config.ListenCheck) * time.Second
	}
	return duration
}

func (l *Listener) Nodes() chains.Nodes {
	return l.sdk.ChainSDK
}

func (l *Listener) LastHeaderSync(uint64, uint64) (uint64, error) {
	return 0, nil
}

func (l *Listener) LatestHeight() (uint64, error) {
	return l.sdk.Node().GetLatestHeight()
}


func (l *Listener) ValidateNodes() (err error) {
	if l.sdk.Delta() <= 0 {
		err = fmt.Errorf("No height increment since last update for chain %d", l.ChainId())
	}
	return
}

func (l *Listener) Validate(tx *msg.Tx) (err error) {
	err = l.validate(l.sdk.Node(), tx)
	if err == nil {
		return
	}
	
	for _, node := range l.sdk.AllNodes() {
		e := l.validate(node, tx)
		if e == nil {
			return
		}
	}
	return
}

func (l *Listener) validate(node *poly.Client, tx *msg.Tx) (err error) {
	t, err := l.scanTx(node, tx.PolyHash)
	if err != nil { return }
	if t == nil {
		return msg.ERR_TX_PROOF_MISSING
	}
	if tx.SrcChainId != t.SrcChainId {
		return fmt.Errorf("%w SrcChainID does not match: %v, was %v", msg.ERR_TX_VOILATION, tx.SrcChainId, t.SrcChainId)
	}
	if tx.DstChainId != t.DstChainId {
		return fmt.Errorf("%w DstChainID does not match: %v, was %v", msg.ERR_TX_VOILATION, tx.DstChainId, t.DstChainId)
	}
	sub := &Submitter{sdk:l.sdk}
	value, _, _, err := sub.getProof(node, t.PolyHeight, t.PolyKey)
	if err != nil { return }
	if value == nil {
		return msg.ERR_TX_PROOF_MISSING
	}
	a := util.LowerHex(hex.EncodeToString(value.MakeTxParam.ToContractAddress))
	b := util.LowerHex(tx.DstProxy)
	if a != b {
		return fmt.Errorf("%w ToContract does not match: %v, was %v", msg.ERR_TX_VOILATION, b, a)
	}
	return
}

func (l *Listener) SDK() *poly.SDK {
	return l.sdk
}