package models

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/smartcontractkit/chainlink/logger"
	"github.com/smartcontractkit/chainlink/store/assets"
	"github.com/smartcontractkit/chainlink/utils"
)

// Descriptive indices of a RunLog's Topic array
const (
	RequestLogTopicSignature = iota
	RequestLogTopicJobID
	RequestLogTopicRequester
	RequestLogTopicAmount
)

const (
	evmWordSize      = common.HashLength
	idSize           = evmWordSize
	versionSize      = evmWordSize
	callbackAddrSize = evmWordSize
	callbackFuncSize = evmWordSize
	expirationSize   = evmWordSize
	dataLocationSize = evmWordSize
	dataLengthSize   = evmWordSize
)

var (
	// RunLogTopic0 was the original topic to filter for Oracle.sol RunRequest events.
	RunLogTopic0 = utils.MustHash("RunRequest(bytes32,address,uint256,uint256,uint256,bytes)")
	// RunLogTopic20190123 was the new RunRequest filter topic as of 2019-01-23,
	// when callback address, callback function, and expiration were added to the data payload.
	RunLogTopic20190123 = utils.MustHash("RunRequest(bytes32,address,uint256,uint256,uint256,address,bytes4,uint256,bytes)")
	// ServiceAgreementExecutionLogTopic is the signature for the
	// Coordinator.RunRequest(...) events which Chainlink nodes watch for. See
	// https://github.com/smartcontractkit/chainlink/blob/master/solidity/contracts/Coordinator.sol#RunRequest
	ServiceAgreementExecutionLogTopic = utils.MustHash("ServiceAgreementExecution(bytes32,address,uint256,uint256,uint256,bytes)")
	// OracleFulfillmentFunctionID0 is the original function selector for fulfilling Ethereum requests.
	OracleFulfillmentFunctionID0 = utils.MustHash("fulfillData(uint256,bytes32)").Hex()[:10]
	// OracleFulfillmentFunctionID20190123 is the function selector for fulfilling Ethereum requests,
	// as updated on 2019-01-23.
	OracleFulfillmentFunctionID20190123 = utils.MustHash("fulfillData(uint256,uint256,address,bytes4,uint256,bytes32)").Hex()[:10]
)

type logRequestParser func(Log) (JSON, error)

// topicFactoryMap maps the log topic to a factory method that returns an
// implementation of the interface LogRequest. The concrete implementations
// are polymorphic and can have difference behaviors for methods like JSON().
var topicFactoryMap = map[common.Hash]logRequestParser{
	ServiceAgreementExecutionLogTopic: parseRunLog0,
	RunLogTopic0:                      parseRunLog0,
	RunLogTopic20190123:               parseRunLog20190123,
}

// TopicFiltersForRunLog generates the two variations of RunLog IDs that could
// possibly be entered on a RunLog or a ServiceAgreementExecutionLog. There is the ID,
// hex encoded and the ID zero padded.
func TopicFiltersForRunLog(logTopics []common.Hash, jobID string) ([][]common.Hash, error) {
	hexJobID := common.BytesToHash([]byte(jobID))
	b, err := hexutil.Decode("0x" + jobID)
	if err != nil {
		return [][]common.Hash{}, fmt.Errorf("Could not hex decode %v: %v", jobID, err)
	}
	jobIDZeroPadded := common.BytesToHash(common.RightPadBytes(b, utils.EVMWordByteLen))
	// LogTopics AND (0xHEXJOBID OR 0xJOBID0padded)
	// i.e. (RunLogTopic0 OR RunLogTopic20190123) AND (0xHEXJOBID OR 0xJOBID0padded)
	return [][]common.Hash{logTopics, {hexJobID, jobIDZeroPadded}}, nil
}

// FilterQueryFactory returns the ethereum FilterQuery for this initiator.
func FilterQueryFactory(i Initiator, from *IndexableBlockNumber) (ethereum.FilterQuery, error) {
	switch i.Type {
	case InitiatorEthLog:
		return newInitiatorFilterQuery(i, from, nil), nil
	case InitiatorRunLog:
		topics := []common.Hash{RunLogTopic20190123, RunLogTopic0}
		filters, err := TopicFiltersForRunLog(topics, i.JobID)
		return newInitiatorFilterQuery(i, from, filters), err
	case InitiatorServiceAgreementExecutionLog:
		topics := []common.Hash{ServiceAgreementExecutionLogTopic}
		filters, err := TopicFiltersForRunLog(topics, i.JobID)
		return newInitiatorFilterQuery(i, from, filters), err
	default:
		return ethereum.FilterQuery{}, fmt.Errorf("Cannot generate a FilterQuery for initiator of type %T", i)
	}
}

func newInitiatorFilterQuery(
	initr Initiator,
	fromBlock *IndexableBlockNumber,
	topics [][]common.Hash,
) ethereum.FilterQuery {
	// Exclude current block from future log subscription to prevent replay.
	listenFromNumber := fromBlock.NextInt()
	q := utils.ToFilterQueryFor(listenFromNumber, []common.Address{initr.Address})
	q.Topics = topics
	return q
}

// LogRequest is the interface to allow polymorphic functionality of different
// types of LogEvents.
// i.e. EthLogEvent, RunLogEvent, ServiceAgreementLogEvent, OracleLogEvent
type LogRequest interface {
	GetLog() Log
	GetJobSpec() JobSpec
	GetInitiator() Initiator

	Validate() bool
	JSON() (JSON, error)
	ToDebug()
	ForLogger(kvs ...interface{}) []interface{}
	ContractPayment() (*assets.Link, error)
	ValidateRequester() error
	ToIndexableBlockNumber() *IndexableBlockNumber
}

// InitiatorLogEvent encapsulates all information as a result of a received log from an
// InitiatorSubscription.
type InitiatorLogEvent struct {
	Log       Log
	JobSpec   JobSpec
	Initiator Initiator
}

// LogRequest is a factory method that coerces this log event to the correct
// type based on Initiator.Type, exposed by the LogRequest interface.
func (le InitiatorLogEvent) LogRequest() LogRequest {
	switch le.Initiator.Type {
	case InitiatorEthLog:
		return EthLogEvent{InitiatorLogEvent: le}
	case InitiatorServiceAgreementExecutionLog:
		fallthrough
	case InitiatorRunLog:
		return RunLogEvent{le}
	}
	logger.Warnw("LogRequest: Unable to discern initiator type for log request", le.ForLogger()...)
	return EthLogEvent{InitiatorLogEvent: le}
}

// GetLog returns the log.
func (le InitiatorLogEvent) GetLog() Log {
	return le.Log
}

// GetJobSpec returns the associated JobSpec
func (le InitiatorLogEvent) GetJobSpec() JobSpec {
	return le.JobSpec
}

// GetInitiator returns the initiator.
func (le InitiatorLogEvent) GetInitiator() Initiator {
	return le.Initiator
}

// ForLogger formats the InitiatorSubscriptionLogEvent for easy common
// formatting in logs (trace statements, not ethereum events).
func (le InitiatorLogEvent) ForLogger(kvs ...interface{}) []interface{} {
	topic := ""
	if len(le.Log.Topics) > 0 {
		topic = le.Log.Topics[0].Hex()
	}
	output := []interface{}{
		"job", le.JobSpec.ID,
		"log", le.Log.BlockNumber,
		"topic0", topic,
		"initiator", le.Initiator,
	}

	return append(kvs, output...)
}

// ToDebug prints this event via logger.Debug.
func (le InitiatorLogEvent) ToDebug() {
	friendlyAddress := utils.LogListeningAddress(le.Initiator.Address)
	msg := fmt.Sprintf("Received log from block #%v for address %v for job %v", le.Log.BlockNumber, friendlyAddress, le.JobSpec.ID)
	logger.Debugw(msg, le.ForLogger()...)
}

// ToIndexableBlockNumber returns an IndexableBlockNumber for the given InitiatorSubscriptionLogEvent Block
func (le InitiatorLogEvent) ToIndexableBlockNumber() *IndexableBlockNumber {
	num := new(big.Int)
	num.SetUint64(le.Log.BlockNumber)
	return NewIndexableBlockNumber(num, le.Log.BlockHash)
}

// Validate returns true, no validation on this log event type.
func (le InitiatorLogEvent) Validate() bool {
	return true
}

// ValidateRequester returns true since all requests are valid for base
// initiator log events.
func (le InitiatorLogEvent) ValidateRequester() error {
	return nil
}

// JSON returns the eth log as JSON.
func (le InitiatorLogEvent) JSON() (JSON, error) {
	el := le.Log
	var out JSON
	b, err := json.Marshal(el)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(b, &out)
}

// ContractPayment returns the amount attached to a contract to pay the Oracle upon fulfillment.
func (le InitiatorLogEvent) ContractPayment() (*assets.Link, error) {
	return nil, nil
}

// EthLogEvent provides functionality specific to a log event emitted
// for an eth log initiator.
type EthLogEvent struct {
	InitiatorLogEvent
}

// RunLogEvent provides functionality specific to a log event emitted
// for a run log initiator.
type RunLogEvent struct {
	InitiatorLogEvent
}

// Validate returns whether or not the contained log has a properly encoded
// job id.
func (le RunLogEvent) Validate() bool {
	el := le.Log
	jid := jobIDFromHexEncodedTopic(el)
	if jid != le.JobSpec.ID && jobIDFromImproperEncodedTopic(el) != le.JobSpec.ID {
		logger.Errorw(fmt.Sprintf("Run Log didn't have matching job ID: %v != %v", jid, le.JobSpec.ID), le.ForLogger()...)
		return false
	}

	return true
}

// ContractPayment returns the amount attached to a contract to pay the Oracle upon fulfillment.
func (le RunLogEvent) ContractPayment() (*assets.Link, error) {
	encodedAmount := le.Log.Topics[RequestLogTopicAmount].Hex()
	payment, ok := new(assets.Link).SetString(encodedAmount, 0)
	if !ok {
		return payment, fmt.Errorf("unable to decoded amount from RunLog: %s", encodedAmount)
	}
	return payment, nil
}

// ValidateRequester returns true if the requester matches the one associated
// with the initiator.
func (le RunLogEvent) ValidateRequester() error {
	if len(le.Initiator.Requesters) == 0 {
		return nil
	}
	for _, r := range le.Initiator.Requesters {
		if le.Requester() == r {
			return nil
		}
	}
	return fmt.Errorf("Run Log didn't have have a valid requester: %v", le.Requester().Hex())
}

// Requester pulls the requesting address out of the LogEvent's topics.
func (le RunLogEvent) Requester() common.Address {
	b := le.Log.Topics[RequestLogTopicRequester].Bytes()
	return common.BytesToAddress(b)
}

// JSON decodes the RunLogEvent's data converts it to a JSON object.
func (le RunLogEvent) JSON() (JSON, error) {
	return ParseRunLog(le.Log)
}

// ParseRunLog decodes the CBOR in the ABI of the log event.
func ParseRunLog(log Log) (JSON, error) {
	topic, err := log.getTopic(0)
	if err != nil {
		return JSON{}, err
	}
	parse, ok := topicFactoryMap[topic]
	if !ok {
		return JSON{}, fmt.Errorf("No parser for the RunLogEvent topic %v", topic)
	}
	return parse(log)
}

func parseRunLog0(log Log) (JSON, error) {
	data := log.Data
	start := idSize + versionSize + dataLocationSize + dataLengthSize

	js, err := ParseCBOR(data[start:])
	if err != nil {
		return js, err
	}
	js, err = js.Add("address", log.Address.String())
	if err != nil {
		return js, err
	}

	js, err = js.Add("dataPrefix", bytesToHex(data[:idSize]))
	if err != nil {
		return js, err
	}

	return js.Add("functionSelector", OracleFulfillmentFunctionID0)
}

func parseRunLog20190123(log Log) (JSON, error) {
	data := log.Data
	cborStart := idSize + versionSize + callbackAddrSize + callbackFuncSize + expirationSize + dataLocationSize + dataLengthSize

	js, err := ParseCBOR(data[cborStart:])
	if err != nil {
		return js, err
	}

	js, err = js.Add("address", log.Address.String())
	if err != nil {
		return js, err
	}

	callbackAndExpStart := idSize + versionSize
	callbackAndExpEnd := callbackAndExpStart + callbackAddrSize + callbackFuncSize + expirationSize
	dataPrefix := bytesToHex(append(append(data[:idSize],
		log.Topics[RequestLogTopicAmount].Bytes()...),
		data[callbackAndExpStart:callbackAndExpEnd]...))
	js, err = js.Add("dataPrefix", dataPrefix)
	if err != nil {
		return js, err
	}

	return js.Add("functionSelector", OracleFulfillmentFunctionID20190123)
}

func bytesToHex(data []byte) string {
	return utils.AddHexPrefix(hex.EncodeToString(data))
}

func decodeABIToJSON(data []byte) (JSON, error) {
	idSize := common.HashLength
	versionSize := common.HashLength
	varLocationSize := common.HashLength
	varLengthSize := common.HashLength
	start := idSize + versionSize + varLocationSize + varLengthSize
	return ParseCBOR(data[start:])
}

func jobIDFromHexEncodedTopic(log Log) string {
	return string(log.Topics[RequestLogTopicJobID].Bytes())
}

func jobIDFromImproperEncodedTopic(log Log) string {
	return log.Topics[RequestLogTopicJobID].String()[2:34]
}
