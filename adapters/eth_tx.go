package adapters

import (
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/smartcontractkit/chainlink/logger"
	"github.com/smartcontractkit/chainlink/store"
	"github.com/smartcontractkit/chainlink/store/models"
	"github.com/smartcontractkit/chainlink/utils"
)

const (
	// DataFormatBytes instructs the EthTx Adapter to treat the input value as a
	// bytes string, rather than a hexadecimal encoded bytes32
	DataFormatBytes = "bytes"
)

// EthTx holds the Address to send the result to and the FunctionSelector
// to execute.
type EthTx struct {
	Address          common.Address          `json:"address"`
	FunctionSelector models.FunctionSelector `json:"functionSelector"`
	DataPrefix       hexutil.Bytes           `json:"dataPrefix"`
	DataFormat       string                  `json:"format"`
	GasPrice         *models.Big             `json:"gasPrice" gorm:"type:varchar(255)"`
	GasLimit         uint64                  `json:"gasLimit"`
}

// Perform creates the run result for the transaction if the existing run result
// is not currently pending. Then it confirms the transaction was confirmed on
// the blockchain.
func (etx *EthTx) Perform(input models.RunResult, store *store.Store) models.RunResult {
	if !store.TxManager.Connected() {
		return input.MarkPendingConnection()
	}

	if !input.Status.PendingConfirmations() {
		return createTxRunResult(etx, input, store)
	}
	return ensureTxRunResult(input, store)
}

// getTxData returns the data to save against the callback encoded according to
// the dataFormat parameter in the job spec
func getTxData(e *EthTx, input models.RunResult) ([]byte, error) {
	value := input.Get("value")
	if e.DataFormat == "" {
		return common.HexToHash(value.Str).Bytes(), nil
	}

	payloadOffset := utils.EVMWordUint64(utils.EVMWordByteLen)
	if len(e.DataPrefix) > 0 {
		payloadOffset = utils.EVMWordUint64(utils.EVMWordByteLen * 2)
	}
	output, err := utils.EVMTranscodeJSONWithFormat(value, e.DataFormat)
	if err != nil {
		return []byte{}, err
	}
	return utils.ConcatBytes(payloadOffset, output)
}

func createTxRunResult(
	e *EthTx,
	input models.RunResult,
	store *store.Store,
) models.RunResult {
	value, err := getTxData(e, input)
	if err != nil {
		return input.WithError(err)
	}

	data, err := utils.ConcatBytes(e.FunctionSelector.Bytes(), e.DataPrefix, value)
	if err != nil {
		return input.WithError(err)
	}

	tx, err := store.TxManager.CreateTxWithGas(e.Address, data, e.GasPrice.ToInt(), e.GasLimit)
	if err != nil {
		return input.WithError(err)
	}

	sendResult := input.WithValue(tx.Hash.String())
	return ensureTxRunResult(sendResult, store)
}

func ensureTxRunResult(input models.RunResult, str *store.Store) models.RunResult {
	val, err := input.Value()
	if err != nil {
		return input.WithError(err)
	}

	hash := common.HexToHash(val)
	if err != nil {
		return input.WithError(err)
	}

	receipt, err := str.TxManager.BumpGasUntilSafe(hash)
	if err != nil {
		logger.Error("EthTx Adapter Perform Resuming: ", err)
	}
	if receipt == nil {
		return input.MarkPendingConfirmations()
	}
	return addReceiptToResult(receipt, input)
}

func addReceiptToResult(receipt *store.TxReceipt, in models.RunResult) models.RunResult {
	receipts := []store.TxReceipt{}

	if !in.Get("ethereumReceipts").IsArray() {
		in = in.Add("ethereumReceipts", receipts)
	}

	if err := json.Unmarshal([]byte(in.Get("ethereumReceipts").String()), &receipts); err != nil {
		logger.Error(fmt.Errorf("EthTx Adapter unmarshaling ethereum Receipts: %v", err))
	}

	receipts = append(receipts, *receipt)
	in = in.Add("ethereumReceipts", receipts)
	return in.WithValue(receipt.Hash.String())
}
