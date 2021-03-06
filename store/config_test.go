package store

import (
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/smartcontractkit/chainlink/store/assets"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func TestStore_ConfigDefaults(t *testing.T) {
	t.Parallel()
	config := NewConfig()
	assert.Equal(t, uint64(0), config.ChainID())
	assert.Equal(t, big.NewInt(20000000000), config.EthGasPriceDefault())
	assert.Equal(t, "0x514910771AF9Ca656af840dff83E8264EcF986CA", common.HexToAddress(config.LinkContractAddress()).String())
	assert.Equal(t, assets.NewLink(1000000000000000000), config.MinimumContractPayment())
	assert.Equal(t, 15*time.Minute, config.SessionTimeout())
	assert.Equal(t, new(url.URL), config.BridgeResponseURL())
}

func TestConfig_sessionSecret(t *testing.T) {
	t.Parallel()
	config := NewConfig()
	config.Set("ROOT", path.Join("/tmp/chainlink_test", fmt.Sprintf("%s", "TestConfig_sessionSecret")))
	err := os.MkdirAll(config.RootDir(), os.FileMode(0770))
	require.NoError(t, err)

	initial, err := config.SessionSecret()
	require.NoError(t, err)
	require.NotEqual(t, "", initial)
	require.NotEqual(t, "clsession_test_secret", initial)

	second, err := config.SessionSecret()
	require.NoError(t, err)
	require.Equal(t, initial, second)
}

func TestConfig_sessionOptions(t *testing.T) {
	t.Parallel()
	config := NewConfig()

	tests := []struct {
		name string
		dev  bool
		want bool
	}{
		{"dev", true, false},
		{"production", false, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config.Set("CHAINLINK_DEV", test.dev)
			opts := config.SessionOptions()
			require.Equal(t, test.want, opts.Secure)
		})
	}
}

func TestConfig_readFromFile(t *testing.T) {
	v := viper.New()
	v.Set("ROOT", "../internal/clroot/")

	config := newConfigWithViper(v)
	assert.Equal(t, config.RootDir(), "../internal/clroot/")
	assert.Equal(t, config.MinOutgoingConfirmations(), uint64(2))
	assert.Equal(t, config.MinimumContractPayment(), assets.NewLink(1000000000000))
	assert.Equal(t, config.Dev(), true)
	assert.Equal(t, config.TLSPort(), uint16(0))
}

func TestStore_addressParser(t *testing.T) {
	zero := &common.Address{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	fifteen := &common.Address{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 15}

	val, err := parseAddress("")
	assert.NoError(t, err)
	assert.Equal(t, nil, val)

	val, err = parseAddress("0x000000000000000000000000000000000000000F")
	assert.NoError(t, err)
	assert.Equal(t, fifteen, val)

	val, err = parseAddress("0X000000000000000000000000000000000000000F")
	assert.NoError(t, err)
	assert.Equal(t, fifteen, val)

	val, err = parseAddress("0")
	assert.NoError(t, err)
	assert.Equal(t, zero, val)

	val, err = parseAddress("15")
	assert.NoError(t, err)
	assert.Equal(t, fifteen, val)

	val, err = parseAddress("0x0")
	assert.Error(t, err)

	val, err = parseAddress("x")
	assert.Error(t, err)
}

func TestStore_bigIntParser(t *testing.T) {
	val, err := parseBigInt("0")
	assert.NoError(t, err)
	assert.Equal(t, new(big.Int).SetInt64(0), val)

	val, err = parseBigInt("15")
	assert.NoError(t, err)
	assert.Equal(t, new(big.Int).SetInt64(15), val)

	val, err = parseBigInt("x")
	assert.Error(t, err)

	val, err = parseBigInt("")
	assert.Error(t, err)
}

func TestStore_levelParser(t *testing.T) {
	val, err := parseLogLevel("ERROR")
	assert.NoError(t, err)
	assert.Equal(t, LogLevel{zapcore.ErrorLevel}, val)

	val, err = parseLogLevel("")
	assert.NoError(t, err)
	assert.Equal(t, LogLevel{zapcore.InfoLevel}, val)

	val, err = parseLogLevel("primus sucks")
	assert.Error(t, err)
}

func TestStore_urlParser(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantError bool
	}{
		{"valid URL", "http://localhost:3000", false},
		{"invalid URL", ":", true},
		{"empty URL", "", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			i, err := parseURL(test.input)

			if test.wantError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				w, ok := i.(*url.URL)
				require.True(t, ok)
				assert.Equal(t, test.input, w.String())
			}
		})
	}
}
