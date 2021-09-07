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
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/beego/beego/v2/core/logs"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ontio/ontology-crypto/keypair"
	"github.com/ontio/ontology-crypto/signature"
	vconf "github.com/ontio/ontology/consensus/vbft/config"
	"github.com/polynetwork/bridge-common/base"
	"github.com/polynetwork/bridge-common/chains/poly"
	"github.com/polynetwork/bridge-common/wallet"
	sdk "github.com/polynetwork/poly-go-sdk"
	scom "github.com/polynetwork/poly-go-sdk/common"
	pcom "github.com/polynetwork/poly/common"
	"github.com/polynetwork/poly/core/types"
	ccom "github.com/polynetwork/poly/native/service/cross_chain_manager/common"

	"github.com/polynetwork/poly-relayer/bus"
	"github.com/polynetwork/poly-relayer/config"
	"github.com/polynetwork/poly-relayer/msg"
)

type Submitter struct {
	context.Context
	wg      *sync.WaitGroup
	config  *config.PolySubmitterConfig
	sdk     *poly.SDK
	signer  *sdk.Account
	name    string
	sync    *config.HeaderSyncConfig
	compose msg.PolyComposer
}

func (s *Submitter) Init(config *config.PolySubmitterConfig) (err error) {
	s.config = config
	s.sdk = poly.WithOptions(config.ChainId, config.Nodes, time.Minute, 1)
	s.signer, err = wallet.NewPolySigner(config.Wallet)
	s.name = base.GetChainName(config.ChainId)
	return
}

func (s *Submitter) SDK() *poly.SDK {
	return s.sdk
}

func (s *Submitter) Submit(msg msg.Message) error {
	return nil
}

func (s *Submitter) Hook(ctx context.Context, wg *sync.WaitGroup, ch <-chan msg.Message) error {
	s.Context = ctx
	s.wg = wg
	return nil
}

func (s *Submitter) SubmitHeadersWithLoop(chainId uint64, headers [][]byte) error {
	for {
		_, err := s.SubmitHeaders(chainId, headers)
		if err == nil {
			return nil
		}
		msg := err.Error()
		if strings.Contains(msg, "parent header not exist") || strings.Contains(msg, "missing required field") {
			//NOTE: reset header height back here
			logs.Error("Possible header fork for chain %d, will rollback some blocks, err %v", chainId, err)
			return err
		}
		logs.Error("Failed to submit side chain(%d) header to poly, err %v", chainId, err)
		time.Sleep(time.Second)
	}
}

func (s *Submitter) SubmitHeaders(chainId uint64, headers [][]byte) (hash string, err error) {
	tx, err := s.sdk.Node().Native.Hs.SyncBlockHeader(
		chainId, s.signer.Address, headers, s.signer,
	)
	if err != nil {
		return "", err
	}
	hash = tx.ToHexString()
	_, err = s.sdk.Node().Confirm(hash, 0, 10)
	if err == nil {
		logs.Info("Submitted side chain(%d) header to poly, hash: %s", chainId, hash)
	}
	return
}

func (s *Submitter) submit(tx *msg.Tx) error {
	// TODO: Check storage to see if already imported
	if tx.SrcHeight == 0 || tx.SrcProof == "" || tx.SrcEvent == "" || tx.SrcChainId == 0 || tx.SrcHash == "" || tx.SrcProofHeight == 0 {
		return fmt.Errorf("Invalid src tx, missing some fields %v", *tx)
	}

	value, err := hex.DecodeString(tx.SrcEvent)
	if err != nil {
		return fmt.Errorf("%s submitter decode src value error %v value %s", s.name, err, tx.SrcEvent)
	}

	proof, err := hex.DecodeString(tx.SrcProof)
	if err != nil {
		return fmt.Errorf("%s submitter decode src proof error %v proof %s", s.name, err, tx.SrcProof)
	}

	t, err := s.sdk.Node().Native.Ccm.ImportOuterTransfer(
		tx.SrcChainId,
		value,
		uint32(tx.SrcProofHeight),
		proof,
		common.Hex2Bytes(s.signer.Address.ToHexString()),
		[]byte{},
		s.signer,
	)
	if err != nil {
		return fmt.Errorf("Failed to import tx to poly, %v", err)
	}
	tx.PolyHash = t.ToHexString()
	return nil
}

func (s *Submitter) ProcessTx(m *msg.Tx, _ msg.PolyComposer) (err error) {
	if m.Type() != msg.SRC {
		return fmt.Errorf("%s desired message is not poly tx %v", m.Type())
	}

	return s.submit(m)
}

func (s *Submitter) Process(msg msg.Message) error {
	return nil
}

func (s *Submitter) Stop() error {
	s.wg.Wait()
	return nil
}

func (s *Submitter) CollectSigs(tx *msg.Tx) (err error) {
	var (
		sigs []byte
	)
	sigHeader := tx.PolyHeader
	if tx.AnchorHeader != nil && tx.AnchorProof != "" {
		sigHeader = tx.AnchorHeader
	}
	for _, sig := range sigHeader.SigData {
		temp := make([]byte, len(sig))
		copy(temp, sig)
		s, err := signature.ConvertToEthCompatible(temp)
		if err != nil {
			return fmt.Errorf("MakeTx signature.ConvertToEthCompatible %v", err)
		}
		sigs = append(sigs, s...)
	}
	tx.DstSigs = sigs
	return
}

func (s *Submitter) ComposeTx(tx *msg.Tx) (err error) {
	if tx.PolyHash == "" {
		return fmt.Errorf("ComposeTx: Invalid poly hash")
	}
	if tx.DstPolyEpochStartHeight == 0 {
		return fmt.Errorf("ComposeTx: Dst chain poly height not specified")
	}

	if tx.PolyHeight == 0 {
		tx.PolyHeight, err = s.sdk.Node().GetBlockHeightByTxHash(tx.PolyHash)
		if err != nil {
			return
		}
	}

	tx.PolyHeader, err = s.sdk.Node().GetHeaderByHeight(tx.PolyHeight + 1)
	if err != nil {
		return err
	}

	var anchorHeight uint32
	if tx.PolyHeight < tx.DstPolyEpochStartHeight {
		anchorHeight = tx.DstPolyEpochStartHeight + 1
	} else {
		isEpoch, _, err := s.CheckEpoch(tx, tx.PolyHeader)
		if err != nil {
			return err
		}
		if isEpoch {
			anchorHeight = tx.PolyHeight + 2
		}
	}

	if anchorHeight > 0 {
		tx.AnchorHeader, err = s.sdk.Node().GetHeaderByHeight(anchorHeight)
		if err != nil {
			return err
		}
		proof, err := s.sdk.Node().GetMerkleProof(tx.PolyHeight+1, anchorHeight)
		if err != nil {
			return err
		}
		tx.AnchorProof = proof.AuditPath
	}

	tx.MerkleValue, tx.AuditPath, _, err = s.GetPolyParams(tx)
	if err != nil {
		return err
	}

	if tx.MerkleValue.MakeTxParam == nil || config.CONFIG.AllowMethod(tx.MerkleValue.MakeTxParam.Method) {
		method := "missing param"
		if tx.Param != nil {
			method = tx.MerkleValue.MakeTxParam.Method
		}
		return fmt.Errorf("%w Invalid poly tx, src chain(%v) tx(%s) method(%s)", msg.ERR_INVALID_TX, tx.SrcChainId, tx.PolyHash, method)
	}

	return s.CollectSigs(tx)
}

func (s *Submitter) GetProof(height uint32, key string) (param *ccom.ToMerkleValue, path []byte, evt *scom.SmartContactEvent, err error) {
	proof, err := s.sdk.Node().GetCrossStatesProof(height, key)
	if err != nil {
		err = fmt.Errorf("GetProof: GetCrossStatesProof error %v", err)
		return
	}
	path, err = hex.DecodeString(proof.AuditPath)
	if err != nil {
		return
	}
	value, _, _, _ := msg.ParseAuditPath(path)
	param = new(ccom.ToMerkleValue)
	err = param.Deserialization(pcom.NewZeroCopySource(value))
	if err != nil {
		err = fmt.Errorf("GetPolyParams: param.Deserialization error %v", err)
	}
	return
}

func (s *Submitter) GetPolyParams(tx *msg.Tx) (param *ccom.ToMerkleValue, path []byte, evt *scom.SmartContactEvent, err error) {
	if tx.PolyHash == "" {
		err = fmt.Errorf("ComposeTx: Invalid poly hash")
		return
	}

	if tx.PolyHeight == 0 {
		tx.PolyHeight, err = s.sdk.Node().GetBlockHeightByTxHash(tx.PolyHash)
		if err != nil {
			return
		}
	}

	if tx.PolyKey != "" {
		return s.GetProof(tx.PolyHeight, tx.PolyKey)
	}

	evt, err = s.sdk.Node().GetSmartContractEvent(tx.PolyHash)
	if err != nil {
		return
	}

	for _, notify := range evt.Notify {
		if notify.ContractAddress == poly.CCM_ADDRESS {
			states := notify.States.([]interface{})
			if len(states) > 5 {
				method, _ := states[0].(string)
				if method == "makeProof" {
					param, path, evt, err = s.GetProof(tx.PolyHeight, states[5].(string))
					if err != nil {
						logs.Error("GetPolyParams: param.Deserialization error %v", err)
					} else {
						return
					}
				}
			}
		}
	}
	err = fmt.Errorf("Valid ToMerkleValue not found")
	return
}

func (s *Submitter) CheckEpoch(tx *msg.Tx, hdr *types.Header) (epoch bool, pubKeys []byte, err error) {
	if len(tx.DstPolyKeepers) == 0 {
		err = fmt.Errorf("Dst chain poly keeper not provided")
		return
	}
	if hdr.NextBookkeeper == pcom.ADDRESS_EMPTY {
		return
	}
	info := &vconf.VbftBlockInfo{}
	err = json.Unmarshal(hdr.ConsensusPayload, info)
	if err != nil {
		err = fmt.Errorf("CheckEpoch consensus payload unmarshal error %v", err)
		return
	}
	var bks []keypair.PublicKey
	for _, peer := range info.NewChainConfig.Peers {
		keyStr, _ := hex.DecodeString(peer.ID)
		key, _ := keypair.DeserializePublicKey(keyStr)
		bks = append(bks, key)
	}
	bks = keypair.SortPublicKeys(bks)
	pubKeys = []byte{}
	sink := pcom.NewZeroCopySink(nil)
	sink.WriteUint64(uint64(len(bks)))
	for _, key := range bks {
		var bytes []byte
		bytes, err = msg.EncodePubKey(key)
		if err != nil {
			return
		}
		pubKeys = append(pubKeys, bytes...)
		bytes, err = msg.EncodeEthPubKey(key)
		if err != nil {
			return
		}
		sink.WriteVarBytes(crypto.Keccak256(bytes[1:])[12:])
	}
	epoch = !bytes.Equal(tx.DstPolyKeepers, sink.Bytes())
	return
}

func (s *Submitter) run(bus bus.TxBus) error {
	s.wg.Add(1)
	defer s.wg.Done()
	for {
		select {
		case <-s.Done():
			logs.Info("%s submitter is exiting now", s.name)
			return nil
		}
		tx, err := bus.Pop(context.Background())
		if err != nil {
			logs.Error("Bus pop error %v", err)
			continue
		}
		if tx == nil {
			time.Sleep(time.Second)
			continue
		}
		err = s.submit(tx)
		if err != nil {
			logs.Error("%s Process poly tx error %v", err)
			tx.Attempts++
			bus.Push(context.Background(), tx)
		}
	}
}

func (s *Submitter) Start(ctx context.Context, wg *sync.WaitGroup, bus bus.TxBus, composer msg.PolyComposer) error {
	s.compose = composer
	s.Context = ctx
	s.wg = wg
	for i := 0; i < s.config.Procs; i++ {
		go s.run(bus)
	}
	return nil
}

func (s *Submitter) StartSync(ctx context.Context, wg *sync.WaitGroup, config *config.HeaderSyncConfig, reset chan<- uint64) (ch chan msg.Header, err error) {
	s.Context = ctx
	s.wg = wg
	s.sync = config

	if s.sync.Batch == 0 {
		s.sync.Batch = 1
	}
	if s.sync.Buffer == 0 {
		s.sync.Buffer = 2 * s.sync.Batch
	}
	if s.sync.Timeout == 0 {
		s.sync.Timeout = 1
	}

	if s.sync.ChainId == 0 {
		return nil, fmt.Errorf("Invalid header sync side chain id")
	}

	ch = make(chan msg.Header, s.sync.Buffer)
	go s.startSync(ch, reset)
	return
}

func (s *Submitter) GetSideChainHeight(chainId uint64) (height uint64, err error) {
	return s.sdk.Node().GetSideChainHeight(chainId)
}

func (s *Submitter) CheckHeaderExistence(header msg.Header) (ok bool, err error) {
	hash, err := s.sdk.Node().GetSideChainHeader(s.sync.ChainId, header.Height)
	if err != nil {
		return
	}
	ok = bytes.Equal(hash, header.Hash)
	return
}

func (s *Submitter) startSync(ch <-chan msg.Header, reset chan<- uint64) {
	if s.sync.Batch == 1 {
		for header := range ch {
			// NOTE err reponse here will revert header sync with delta -100
			ok, err := s.CheckHeaderExistence(header)
			if ok {
				continue
			}
			if err == nil {
				err = s.SubmitHeadersWithLoop(s.sync.ChainId, [][]byte{header.Data})
			}
			if err != nil {
				reset <- header.Height - 100
			}
		}
	} else {
		headers := [][]byte{}
		commit := false
		duration := time.Duration(s.sync.Timeout) * time.Second
		var height uint64
	COMMIT:
		for {
			select {
			case header, ok := <-ch:
				if ok {
					height = header.Height
					headers = append(headers, header.Data)
					commit = len(headers) >= s.sync.Batch
				} else {
					commit = len(headers) > 0
					break COMMIT
				}
			case <-time.After(duration):
				commit = len(headers) > 0
			}
			if commit {
				commit = false
				// NOTE err reponse here will revert header sync with delta -100
				err := s.SubmitHeadersWithLoop(s.sync.ChainId, headers)
				if err != nil {
					reset <- height - 100 - uint64(len(headers))
				}
				headers = [][]byte{}
			}
		}
		if len(headers) > 0 {
			s.SubmitHeaders(s.sync.ChainId, headers)
		}
	}
	logs.Info("Header sync exiting loop now")
}
