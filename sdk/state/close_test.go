package state

import (
	"testing"
	"time"

	"github.com/stellar/experimental-payment-channels/sdk/txbuildtest"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannel_CloseTx(t *testing.T) {
	localSigner := keypair.MustRandom()
	remoteSigner := keypair.MustRandom()
	localEscrowAccount := &EscrowAccount{
		Address:        keypair.MustRandom().FromAddress(),
		SequenceNumber: int64(101),
	}
	remoteEscrowAccount := &EscrowAccount{
		Address:        keypair.MustRandom().FromAddress(),
		SequenceNumber: int64(202),
	}

	channel := NewChannel(Config{
		NetworkPassphrase:   network.TestNetworkPassphrase,
		Initiator:           true,
		LocalSigner:         localSigner,
		RemoteSigner:        remoteSigner.FromAddress(),
		LocalEscrowAccount:  localEscrowAccount,
		RemoteEscrowAccount: remoteEscrowAccount,
	})
	channel.openAgreement = OpenAgreement{
		Details: OpenAgreementDetails{
			ObservationPeriodTime:      1,
			ObservationPeriodLedgerGap: 1,
			Asset:                      NativeAsset,
			ExpiresAt:                  time.Now(),
		},
	}
	channel.latestAuthorizedCloseAgreement = CloseAgreement{
		Details: CloseAgreementDetails{
			ObservationPeriodTime:      1,
			ObservationPeriodLedgerGap: 2,
			IterationNumber:            3,
			Balance:                    4,
			ProposingSigner:            localSigner.FromAddress(),
			ConfirmingSigner:           remoteSigner.FromAddress(),
		},
		ProposerSignatures: CloseAgreementSignatures{
			Declaration: xdr.Signature{0},
			Close:       xdr.Signature{1},
		},
		ConfirmerSignatures: CloseAgreementSignatures{
			Declaration: xdr.Signature{2},
			Close:       xdr.Signature{3},
		},
	}

	declTx, closeTx, err := channel.CloseTxs()
	require.NoError(t, err)
	closeTxHash, err := closeTx.Hash(channel.networkPassphrase)
	require.NoError(t, err)
	// TODO: Compare the non-signature parts of the txs with the result of
	// channel.closeTxs() when there is an practical way of doing that added to
	// txnbuild.
	assert.ElementsMatch(t, []xdr.DecoratedSignature{
		{Hint: localSigner.Hint(), Signature: []byte{0}},
		{Hint: remoteSigner.Hint(), Signature: []byte{2}},
		xdr.NewDecoratedSignatureForPayload([]byte{3}, remoteSigner.Hint(), closeTxHash[:]),
	}, declTx.Signatures())
	assert.ElementsMatch(t, []xdr.DecoratedSignature{
		{Hint: localSigner.Hint(), Signature: []byte{1}},
		{Hint: remoteSigner.Hint(), Signature: []byte{3}},
	}, closeTx.Signatures())
}

func TestChannel_ProposeClose(t *testing.T) {
	localSigner := keypair.MustRandom()
	remoteSigner := keypair.MustRandom()
	localEscrowAccount := &EscrowAccount{
		Address:        keypair.MustRandom().FromAddress(),
		SequenceNumber: int64(101),
	}
	remoteEscrowAccount := &EscrowAccount{
		Address:        keypair.MustRandom().FromAddress(),
		SequenceNumber: int64(202),
	}

	localChannel := NewChannel(Config{
		NetworkPassphrase:   network.TestNetworkPassphrase,
		Initiator:           true,
		LocalSigner:         localSigner,
		RemoteSigner:        remoteSigner.FromAddress(),
		LocalEscrowAccount:  localEscrowAccount,
		RemoteEscrowAccount: remoteEscrowAccount,
		MaxOpenExpiry:       2 * time.Hour,
	})
	remoteChannel := NewChannel(Config{
		NetworkPassphrase:   network.TestNetworkPassphrase,
		Initiator:           false,
		LocalSigner:         remoteSigner,
		RemoteSigner:        localSigner.FromAddress(),
		LocalEscrowAccount:  remoteEscrowAccount,
		RemoteEscrowAccount: localEscrowAccount,
		MaxOpenExpiry:       2 * time.Hour,
	})

	// Put channel into the Open state.
	{
		open1, err := localChannel.ProposeOpen(OpenParams{
			ObservationPeriodTime:      1,
			ObservationPeriodLedgerGap: 1,
			ExpiresAt:                  time.Now().Add(time.Hour),
		})
		require.NoError(t, err)
		open2, err := remoteChannel.ConfirmOpen(open1)
		require.NoError(t, err)
		_, err = localChannel.ConfirmOpen(open2)
		require.NoError(t, err)

		ftx, err := localChannel.OpenTx()
		require.NoError(t, err)
		ftxXDR, err := ftx.Base64()
		require.NoError(t, err)

		successResultXDR, err := txbuildtest.BuildResultXDR(true)
		require.NoError(t, err)
		resultMetaXDR, err := txbuildtest.BuildFormationResultMetaXDR(txbuildtest.FormationResultMetaParams{
			InitiatorSigner: localSigner.Address(),
			ResponderSigner: remoteSigner.Address(),
			InitiatorEscrow: localEscrowAccount.Address.Address(),
			ResponderEscrow: remoteEscrowAccount.Address.Address(),
			StartSequence:   localEscrowAccount.SequenceNumber + 1,
			Asset:           txnbuild.NativeAsset{},
		})
		require.NoError(t, err)

		err = localChannel.IngestTx(ftxXDR, successResultXDR, resultMetaXDR)
		require.NoError(t, err)
		err = remoteChannel.IngestTx(ftxXDR, successResultXDR, resultMetaXDR)
		require.NoError(t, err)
	}

	// If the local proposes a close, the agreement will have them as the proposer.
	closeByLocal, err := localChannel.ProposeClose()
	require.NoError(t, err)
	assert.Equal(t, localSigner.FromAddress(), closeByLocal.Details.ProposingSigner)
	assert.Equal(t, remoteSigner.FromAddress(), closeByLocal.Details.ConfirmingSigner)

	// If the remote proposes a close, the agreement will have them as the proposer.
	closeByRemote, err := remoteChannel.ProposeClose()
	require.NoError(t, err)
	assert.Equal(t, remoteSigner.FromAddress(), closeByRemote.Details.ProposingSigner)
	assert.Equal(t, localSigner.FromAddress(), closeByRemote.Details.ConfirmingSigner)
}

func TestChannel_ProposeAndConfirmCoordinatedClose_rejectIfChannelNotOpen(t *testing.T) {
	localSigner := keypair.MustRandom()
	remoteSigner := keypair.MustRandom()
	localEscrowAccount := &EscrowAccount{
		Address:        keypair.MustRandom().FromAddress(),
		SequenceNumber: int64(101),
		Balance:        int64(100),
	}
	remoteEscrowAccount := &EscrowAccount{
		Address:        keypair.MustRandom().FromAddress(),
		SequenceNumber: int64(202),
		Balance:        int64(100),
	}

	senderChannel := NewChannel(Config{
		NetworkPassphrase:   network.TestNetworkPassphrase,
		Initiator:           true,
		MaxOpenExpiry:       10 * time.Second,
		LocalSigner:         localSigner,
		RemoteSigner:        remoteSigner.FromAddress(),
		LocalEscrowAccount:  localEscrowAccount,
		RemoteEscrowAccount: remoteEscrowAccount,
	})

	// Before open, proposing a coordinated close should error.
	_, err := senderChannel.ProposeClose()
	require.EqualError(t, err, "cannot propose a coordinated close before channel is opened")

	// Before open, confirming a coordinated close should error.
	_, err = senderChannel.ConfirmClose(CloseAgreement{})
	require.EqualError(t, err, "validating close agreement: cannot confirm a coordinated close before channel is opened")
}
