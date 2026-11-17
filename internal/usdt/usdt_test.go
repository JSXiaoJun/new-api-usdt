package usdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTronAddressRoundTripsTheOfficialUSDTContract(t *testing.T) {
	tron, ok := Lookup(ChainTron)
	require.True(t, ok)
	raw, err := ParseHex20(tron.USDT)
	require.NoError(t, err)
	assert.Equal(t, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", TronAddress(raw))

	back, err := ParseTronAddress("TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	require.NoError(t, err)
	assert.Equal(t, raw, back)

	_, err = ParseTronAddress("TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6u")
	assert.Error(t, err, "checksum must be enforced")
}

func TestDisplayAddressUsesEIP55Checksum(t *testing.T) {
	bsc, _ := Lookup(ChainBSC)
	raw, err := ParseHex20(bsc.USDT)
	require.NoError(t, err)
	assert.Equal(t, "0x55d398326f99059fF775485246999027B3197955", DisplayAddress(bsc, raw))
}
