package state_test

import (
	"fmt"
	"testing"

	"github.com/stellar/experimental-payment-channels/sdk/state"
	"github.com/stellar/go/txnbuild"
	"github.com/stretchr/testify/assert"
)

func TestAsset(t *testing.T) {
	testCases := []struct {
		Asset             state.Asset
		WantTxnbuildAsset txnbuild.Asset
		WantIsNative      bool
		WantCode          string
		WantIssuer        string
	}{
		{state.Asset(""), txnbuild.NativeAsset{}, true, "", ""},
		{state.Asset("native"), txnbuild.NativeAsset{}, true, "", ""},
		{state.NativeAsset, txnbuild.NativeAsset{}, true, "", ""},
		{state.Asset(":"), txnbuild.CreditAsset{}, false, "", ""},
		{state.Asset("ABCD:GABCD"), txnbuild.CreditAsset{Code: "ABCD", Issuer: "GABCD"}, false, "ABCD", "GABCD"},
		{state.Asset("ABCD:GABCD:AB"), txnbuild.CreditAsset{Code: "ABCD", Issuer: "GABCD:AB"}, false, "ABCD", "GABCD:AB"},
	}
	for _, tc := range testCases {
		t.Run(fmt.Sprint(tc.Asset), func(t *testing.T) {
			assert.Equal(t, tc.WantTxnbuildAsset, tc.Asset.Asset())
			assert.Equal(t, tc.WantIsNative, tc.Asset.IsNative())
			assert.Equal(t, tc.WantCode, tc.Asset.Code())
			assert.Equal(t, tc.WantIssuer, tc.Asset.Issuer())
		})
	}
}