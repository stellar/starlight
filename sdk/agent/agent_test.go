package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
	"github.com/stellar/starlight/sdk/state"
	"github.com/stellar/starlight/sdk/txbuild/txbuildtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sequenceNumberCollector func(accountID *keypair.FromAddress) (int64, error)

func (f sequenceNumberCollector) GetSequenceNumber(accountID *keypair.FromAddress) (int64, error) {
	return f(accountID)
}

type balanceCollectorFunc func(accountID *keypair.FromAddress, asset state.Asset) (int64, error)

func (f balanceCollectorFunc) GetBalance(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
	return f(accountID, asset)
}

type submitterFunc func(tx *txnbuild.Transaction) error

func (f submitterFunc) SubmitTx(tx *txnbuild.Transaction) error {
	return f(tx)
}

type streamerFunc func(cursor string, accounts ...*keypair.FromAddress) (transactions <-chan StreamedTransaction, cancel func())

func (f streamerFunc) StreamTx(cursor string, accounts ...*keypair.FromAddress) (transactions <-chan StreamedTransaction, cancel func()) {
	return f(cursor, accounts...)
}

type snapshotterFunc func(a *Agent, s Snapshot)

func (f snapshotterFunc) Snapshot(a *Agent, s Snapshot) {
	f(a, s)
}

func assertAgentSnapshotsAndRestores(t *testing.T, agent *Agent, config Config, snapshot Snapshot) {
	t.Helper()

	snapshotJSON, err := json.Marshal(snapshot)
	require.NoError(t, err)
	restoredSnapshot := Snapshot{}
	err = json.Unmarshal(snapshotJSON, &restoredSnapshot)
	require.NoError(t, err)

	// Override the streamer so that multiple agents aren't competing to read
	// from the same ingestion streamer.
	config.Streamer = streamerFunc(func(cursor string, accounts ...*keypair.FromAddress) (transactions <-chan StreamedTransaction, cancel func()) {
		// Create a closed channel since we won't be doing any ingestion with this agent.
		txs := make(chan StreamedTransaction)
		close(txs)
		return txs, func() {}
	})

	restoredAgent := NewAgentFromSnapshot(config, snapshot)

	// Check that fields that store state in the agent are the same after
	// restoring.
	assert.Equal(t, agent.observationPeriodTime, restoredAgent.observationPeriodTime)
	assert.Equal(t, agent.observationPeriodLedgerGap, restoredAgent.observationPeriodLedgerGap)
	assert.Equal(t, agent.maxOpenExpiry, restoredAgent.maxOpenExpiry)
	assert.Equal(t, agent.networkPassphrase, restoredAgent.networkPassphrase)
	assert.Equal(t, agent.channelAccountKey, restoredAgent.channelAccountKey)
	assert.Equal(t, agent.channelAccountSigner, restoredAgent.channelAccountSigner)
	assert.Equal(t, agent.otherChannelAccount, restoredAgent.otherChannelAccount)
	assert.Equal(t, agent.otherChannelAccountSigner, restoredAgent.otherChannelAccountSigner)
	assert.Equal(t, agent.channel, restoredAgent.channel)
	assert.Equal(t, agent.streamerCursor, restoredAgent.streamerCursor)
}

func TestAgent_openPaymentClose(t *testing.T) {
	localChannelAccount := keypair.MustParseAddress("GAU4CFXQI6HLK5PPY2JWU3GMRJIIQNLF24XRAHX235F7QTG6BEKLGQ36")
	localSigner := keypair.MustParseFull("SCBMAMOPWKL2YHWELK63VLAY2R74A6GTLLD4ON223B7K5KZ37MUR6IDF")
	remoteChannelAccount := keypair.MustParseAddress("GBQNGSEHTFC4YGQ3EXHIL7JQBA6265LFANKFFAYKHM7JFGU5CORROEGO")
	remoteSigner := keypair.MustParseFull("SBM7D2IIDSRX5Y3VMTMTXXPB6AIB4WYGZBC2M64U742BNOK32X6SW4NF")

	// Setup the local agent.
	localVars := struct {
		submittedTx        *txnbuild.Transaction
		transactionsStream chan StreamedTransaction
	}{}
	localVars.transactionsStream = make(chan StreamedTransaction)
	localEvents := make(chan interface{}, 1)
	localConfig := Config{
		ObservationPeriodTime:      20 * time.Second,
		ObservationPeriodLedgerGap: 1,
		MaxOpenExpiry:              5 * time.Minute,
		NetworkPassphrase:          network.TestNetworkPassphrase,
		SequenceNumberCollector: sequenceNumberCollector(func(accountID *keypair.FromAddress) (int64, error) {
			if accountID.Equal(localChannelAccount) {
				return 28037546508288, nil
			}
			if accountID.Equal(remoteChannelAccount) {
				return 28054726377472, nil
			}
			return 0, fmt.Errorf("unknown channel account")
		}),
		BalanceCollector: balanceCollectorFunc(func(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
			return 100_0000000, nil
		}),
		Submitter: submitterFunc(func(tx *txnbuild.Transaction) error {
			localVars.submittedTx = tx
			return nil
		}),
		Streamer: streamerFunc(func(cursor string, accounts ...*keypair.FromAddress) (transactions <-chan StreamedTransaction, cancel func()) {
			return localVars.transactionsStream, func() {}
		}),
		ChannelAccountKey:    localChannelAccount.FromAddress(),
		ChannelAccountSigner: localSigner,
		LogWriter:            io.Discard,
		Events:               localEvents,
	}
	localConfig.Snapshotter = snapshotterFunc(func(a *Agent, s Snapshot) {
		assertAgentSnapshotsAndRestores(t, a, localConfig, s)
	})
	localAgent := NewAgent(localConfig)

	// Setup the remote agent.
	remoteVars := struct {
		submittedTx        *txnbuild.Transaction
		transactionsStream chan StreamedTransaction
	}{}
	remoteVars.transactionsStream = make(chan StreamedTransaction)
	remoteEvents := make(chan interface{}, 1)
	remoteConfig := Config{
		ObservationPeriodTime:      20 * time.Second,
		ObservationPeriodLedgerGap: 1,
		MaxOpenExpiry:              5 * time.Minute,
		NetworkPassphrase:          network.TestNetworkPassphrase,
		SequenceNumberCollector: sequenceNumberCollector(func(accountID *keypair.FromAddress) (int64, error) {
			if accountID.Equal(localChannelAccount) {
				return 28037546508288, nil
			}
			if accountID.Equal(remoteChannelAccount) {
				return 28054726377472, nil
			}
			return 0, fmt.Errorf("unknown channel account")
		}),
		BalanceCollector: balanceCollectorFunc(func(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
			return 100_0000000, nil
		}),
		Submitter: submitterFunc(func(tx *txnbuild.Transaction) error {
			remoteVars.submittedTx = tx
			return nil
		}),
		Streamer: streamerFunc(func(cursor string, accounts ...*keypair.FromAddress) (transactions <-chan StreamedTransaction, cancel func()) {
			return remoteVars.transactionsStream, func() {}
		}),
		ChannelAccountKey:    remoteChannelAccount.FromAddress(),
		ChannelAccountSigner: remoteSigner,
		LogWriter:            io.Discard,
		Events:               remoteEvents,
	}
	remoteConfig.Snapshotter = snapshotterFunc(func(a *Agent, s Snapshot) {
		assertAgentSnapshotsAndRestores(t, a, remoteConfig, s)
	})
	remoteAgent := NewAgent(remoteConfig)

	// Connect the two agents.
	type ReadWriter struct {
		io.Reader
		io.Writer
	}
	localMsgs := bytes.Buffer{}
	remoteMsgs := bytes.Buffer{}
	localAgent.conn = ReadWriter{
		Reader: &remoteMsgs,
		Writer: &localMsgs,
	}
	remoteAgent.conn = ReadWriter{
		Reader: &localMsgs,
		Writer: &remoteMsgs,
	}
	err := localAgent.hello()
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.NoError(t, err)
	err = remoteAgent.hello()
	require.NoError(t, err)
	err = localAgent.receive()
	require.NoError(t, err)

	// Expect connected event.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		assert.IsType(t, ConnectedEvent{}, localEvent)
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		assert.IsType(t, ConnectedEvent{}, remoteEvent)
	}

	// Extra hellos are allowed and have no consequence.
	err = localAgent.hello()
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.NoError(t, err)

	// Expect connected event.
	{
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		assert.IsType(t, ConnectedEvent{}, remoteEvent)
	}

	// Extra hellos with wrong data raise an error.
	incorrectChannelAccount := keypair.MustRandom().FromAddress()
	localAgent.channelAccountKey = incorrectChannelAccount
	err = localAgent.hello()
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.EqualError(t, err, "handling message: handling message 10: hello received with unexpected channel account: "+incorrectChannelAccount.Address()+" expected: "+localChannelAccount.Address())
	localAgent.channelAccountKey = localChannelAccount

	// Expect error event.
	{
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		assert.IsType(t, ErrorEvent{}, remoteEvent)
	}

	// Extra hellos with wrong data raise an error.
	incorrectSigner := keypair.MustRandom()
	localAgent.channelAccountSigner = incorrectSigner
	err = localAgent.hello()
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.EqualError(t, err, "handling message: handling message 10: hello received with unexpected signer: "+incorrectSigner.Address()+" expected: "+localSigner.Address())
	localAgent.channelAccountSigner = localSigner

	// Expect error event.
	{
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		assert.IsType(t, ErrorEvent{}, remoteEvent)
	}

	// Open the channel.
	err = localAgent.Open(state.NativeAsset)
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.NoError(t, err)
	err = localAgent.receive()
	require.NoError(t, err)

	// Expect the open tx to have been submitted.
	openTx, err := localAgent.channel.OpenTx()
	require.NoError(t, err)
	assert.Equal(t, openTx, localVars.submittedTx)
	localVars.submittedTx = nil

	// Ingest the submitted open tx, as if it was processed on network.
	openTxXDR, err := openTx.Base64()
	require.NoError(t, err)
	openTxStreamed := StreamedTransaction{
		TransactionXDR: openTxXDR,
		ResultXDR: func() string {
			r, err := txbuildtest.BuildResultXDR(true)
			require.NoError(t, err)
			return r
		}(),
		ResultMetaXDR: func() string {
			r, err := txbuildtest.BuildOpenResultMetaXDR(txbuildtest.OpenResultMetaParams{
				InitiatorSigner:         localSigner.Address(),
				ResponderSigner:         remoteSigner.Address(),
				InitiatorChannelAccount: localChannelAccount.Address(),
				ResponderChannelAccount: remoteChannelAccount.Address(),
				StartSequence:           28037546508289,
				Asset:                   txnbuild.NativeAsset{},
			})
			require.NoError(t, err)
			return r
		}(),
	}
	localVars.transactionsStream <- openTxStreamed
	remoteVars.transactionsStream <- openTxStreamed

	// Expect opened event.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		assert.IsType(t, OpenedEvent{}, localEvent)
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		assert.IsType(t, OpenedEvent{}, remoteEvent)
	}

	// Make a payment.
	err = localAgent.Payment(50_0000000)
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.NoError(t, err)
	err = localAgent.receive()
	require.NoError(t, err)

	// Expect payment events.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		localPaymentEvent, ok := localEvent.(PaymentSentEvent)
		require.True(t, ok)
		assert.Equal(t, int64(2), localPaymentEvent.CloseAgreement.Envelope.Details.IterationNumber)
		assert.Equal(t, int64(50_0000000), localPaymentEvent.CloseAgreement.Envelope.Details.Balance)
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		remotePaymentEvent, ok := remoteEvent.(PaymentReceivedEvent)
		require.True(t, ok)
		assert.Equal(t, int64(2), remotePaymentEvent.CloseAgreement.Envelope.Details.IterationNumber)
		assert.Equal(t, int64(50_0000000), remotePaymentEvent.CloseAgreement.Envelope.Details.Balance)
	}

	// Make another payment.
	err = remoteAgent.Payment(20_0000000)
	require.NoError(t, err)
	err = localAgent.receive()
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.NoError(t, err)

	// Expect payment events.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		localPaymentEvent, ok := localEvent.(PaymentReceivedEvent)
		require.True(t, ok)
		assert.Equal(t, int64(3), localPaymentEvent.CloseAgreement.Envelope.Details.IterationNumber)
		assert.Equal(t, int64(30_0000000), localPaymentEvent.CloseAgreement.Envelope.Details.Balance)
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		remotePaymentEvent, ok := remoteEvent.(PaymentSentEvent)
		require.True(t, ok)
		assert.Equal(t, int64(3), remotePaymentEvent.CloseAgreement.Envelope.Details.IterationNumber)
		assert.Equal(t, int64(30_0000000), remotePaymentEvent.CloseAgreement.Envelope.Details.Balance)
	}

	// Make a payment with a memo.
	err = remoteAgent.PaymentWithMemo(20_0000000, []byte("memo"))
	require.NoError(t, err)
	err = localAgent.receive()
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.NoError(t, err)

	// Expect payment events.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		localPaymentEvent, ok := localEvent.(PaymentReceivedEvent)
		require.True(t, ok)
		assert.Equal(t, []byte("memo"), localPaymentEvent.CloseAgreement.Envelope.Details.Memo)
		assert.Equal(t, int64(4), localPaymentEvent.CloseAgreement.Envelope.Details.IterationNumber)
		assert.Equal(t, int64(10_0000000), localPaymentEvent.CloseAgreement.Envelope.Details.Balance)
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		remotePaymentEvent, ok := remoteEvent.(PaymentSentEvent)
		require.True(t, ok)
		assert.Equal(t, []byte("memo"), remotePaymentEvent.CloseAgreement.Envelope.Details.Memo)
		assert.Equal(t, int64(4), remotePaymentEvent.CloseAgreement.Envelope.Details.IterationNumber)
		assert.Equal(t, int64(10_0000000), remotePaymentEvent.CloseAgreement.Envelope.Details.Balance)
	}

	// Make a payment with a memo that is underfunded, but will become funded
	// when updating balance.
	localAgent.balanceCollector = balanceCollectorFunc(func(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
		return 300_0000000, nil
	})
	remoteAgent.balanceCollector = balanceCollectorFunc(func(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
		return 300_0000000, nil
	})
	err = remoteAgent.PaymentWithMemo(200_0000000, []byte("memo"))
	require.NoError(t, err)
	err = localAgent.receive()
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.NoError(t, err)

	// Expect payment events.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		localPaymentEvent, ok := localEvent.(PaymentReceivedEvent)
		require.True(t, ok)
		assert.Equal(t, []byte("memo"), localPaymentEvent.CloseAgreement.Envelope.Details.Memo)
		assert.Equal(t, int64(5), localPaymentEvent.CloseAgreement.Envelope.Details.IterationNumber)
		assert.Equal(t, int64(-190_0000000), localPaymentEvent.CloseAgreement.Envelope.Details.Balance)
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		remotePaymentEvent, ok := remoteEvent.(PaymentSentEvent)
		require.True(t, ok)
		assert.Equal(t, []byte("memo"), remotePaymentEvent.CloseAgreement.Envelope.Details.Memo)
		assert.Equal(t, int64(5), remotePaymentEvent.CloseAgreement.Envelope.Details.IterationNumber)
		assert.Equal(t, int64(-190_0000000), remotePaymentEvent.CloseAgreement.Envelope.Details.Balance)
	}

	// Expect no txs to have been submitted for payments.
	assert.Nil(t, localVars.submittedTx)
	assert.Nil(t, remoteVars.submittedTx)

	// Declare the close, and start negotiating for an early close.
	err = localAgent.DeclareClose()
	require.NoError(t, err)

	// Expect the declaration tx to have been submitted.
	localDeclTx, _, err := localAgent.channel.CloseTxs()
	require.NoError(t, err)
	assert.Equal(t, localDeclTx, localVars.submittedTx)

	// Ingest the local submitted declaration tx, as if it was processed on
	// network.
	localDeclTxXDR, err := localDeclTx.Base64()
	require.NoError(t, err)
	localDeclTxStreamed := StreamedTransaction{
		TransactionXDR: localDeclTxXDR,
		ResultXDR: func() string {
			r, err := txbuildtest.BuildResultXDR(true)
			require.NoError(t, err)
			return r
		}(),
		ResultMetaXDR: func() string {
			r, err := txbuildtest.BuildResultMetaXDR([]xdr.LedgerEntryData{
				{
					Type: xdr.LedgerEntryTypeAccount,
					Account: &xdr.AccountEntry{
						AccountId: xdr.MustAddress(localChannelAccount.Address()),
						SeqNum:    xdr.SequenceNumber(localDeclTx.SequenceNumber()),
						Signers: []xdr.Signer{
							{Key: xdr.MustSigner(localSigner.Address()), Weight: 1},
							{Key: xdr.MustSigner(remoteSigner.Address()), Weight: 1},
						},
						Thresholds: xdr.Thresholds{0, 2, 2, 2},
					},
				},
				{
					Type: xdr.LedgerEntryTypeAccount,
					Account: &xdr.AccountEntry{
						AccountId: xdr.MustAddress(remoteChannelAccount.Address()),
						SeqNum:    28054726377472,
						Signers: []xdr.Signer{
							{Key: xdr.MustSigner(remoteSigner.Address()), Weight: 1},
							{Key: xdr.MustSigner(localSigner.Address()), Weight: 1},
						},
						Thresholds: xdr.Thresholds{0, 2, 2, 2},
					},
				},
			})
			require.NoError(t, err)
			return r
		}(),
	}
	localVars.transactionsStream <- localDeclTxStreamed
	remoteVars.transactionsStream <- localDeclTxStreamed

	// Expect closing event.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		assert.Equal(t, localEvent, ClosingEvent{})
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		assert.Equal(t, remoteEvent, ClosingEvent{})
	}

	// Receive the declaration at the remote and complete negotiation.
	err = remoteAgent.receive()
	require.NoError(t, err)
	err = localAgent.receive()
	require.NoError(t, err)

	// Expect the close tx to have been submitted.
	_, localCloseTx, err := localAgent.channel.CloseTxs()
	require.NoError(t, err)
	_, remoteCloseTx, err := remoteAgent.channel.CloseTxs()
	require.NoError(t, err)
	assert.Equal(t, localCloseTx, remoteCloseTx)
	assert.Equal(t, localCloseTx, localVars.submittedTx)
	assert.Equal(t, remoteCloseTx, remoteVars.submittedTx)

	// Ingest the local submitted close tx, as if it was processed on network.
	// Assume the local submitted successfully first, so the remote did not
	// succeed, and so both local and remote see the transaction submitted by
	// the local.
	localCloseTxXDR, err := localCloseTx.Base64()
	require.NoError(t, err)
	localCloseTxStreamed := StreamedTransaction{
		TransactionXDR: localCloseTxXDR,
		ResultXDR: func() string {
			r, err := txbuildtest.BuildResultXDR(true)
			require.NoError(t, err)
			return r
		}(),
		ResultMetaXDR: func() string {
			r, err := txbuildtest.BuildResultMetaXDR([]xdr.LedgerEntryData{
				{
					Type: xdr.LedgerEntryTypeAccount,
					Account: &xdr.AccountEntry{
						AccountId: xdr.MustAddress(localChannelAccount.Address()),
						SeqNum:    xdr.SequenceNumber(localCloseTx.SequenceNumber()),
						Signers: []xdr.Signer{
							{Key: xdr.MustSigner(localSigner.Address()), Weight: 1},
						},
						Thresholds: xdr.Thresholds{0, 1, 1, 1},
					},
				},
				{
					Type: xdr.LedgerEntryTypeAccount,
					Account: &xdr.AccountEntry{
						AccountId: xdr.MustAddress(remoteChannelAccount.Address()),
						SeqNum:    28054726377472,
						Signers: []xdr.Signer{
							{Key: xdr.MustSigner(remoteSigner.Address()), Weight: 1},
						},
						Thresholds: xdr.Thresholds{0, 1, 1, 1},
					},
				},
			})
			require.NoError(t, err)
			return r
		}(),
	}
	localVars.transactionsStream <- localCloseTxStreamed
	remoteVars.transactionsStream <- localCloseTxStreamed

	// Expect closed event.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		assert.Equal(t, localEvent, ClosedEvent{})
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		assert.Equal(t, remoteEvent, ClosedEvent{})
	}
}

func TestAgent_concurrency(t *testing.T) {
	localChannelAccount := keypair.MustParseAddress("GAU4CFXQI6HLK5PPY2JWU3GMRJIIQNLF24XRAHX235F7QTG6BEKLGQ36")
	localSigner := keypair.MustParseFull("SCBMAMOPWKL2YHWELK63VLAY2R74A6GTLLD4ON223B7K5KZ37MUR6IDF")
	remoteChannelAccount := keypair.MustParseAddress("GBQNGSEHTFC4YGQ3EXHIL7JQBA6265LFANKFFAYKHM7JFGU5CORROEGO")
	remoteSigner := keypair.MustParseFull("SBM7D2IIDSRX5Y3VMTMTXXPB6AIB4WYGZBC2M64U742BNOK32X6SW4NF")

	localVars := struct {
		transactionsStream chan StreamedTransaction
	}{}
	localVars.transactionsStream = make(chan StreamedTransaction)
	remoteVars := struct {
		transactionsStream chan StreamedTransaction
	}{}
	remoteVars.transactionsStream = make(chan StreamedTransaction)

	// Setup the local agent.
	localAgent := &Agent{
		observationPeriodTime:      20 * time.Second,
		observationPeriodLedgerGap: 1,
		maxOpenExpiry:              5 * time.Minute,
		networkPassphrase:          network.TestNetworkPassphrase,
		sequenceNumberCollector: sequenceNumberCollector(func(accountID *keypair.FromAddress) (int64, error) {
			if accountID.Equal(localChannelAccount) {
				return 28037546508288, nil
			}
			if accountID.Equal(remoteChannelAccount) {
				return 28054726377472, nil
			}
			return 0, fmt.Errorf("unknown channel account")
		}),
		balanceCollector: balanceCollectorFunc(func(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
			return 100_0000000, nil
		}),
		submitter: submitterFunc(func(tx *txnbuild.Transaction) error {
			txXDR, err := tx.Base64()
			require.NoError(t, err)
			streamedTx := StreamedTransaction{
				TransactionXDR: txXDR,
				ResultXDR: func() string {
					r, err := txbuildtest.BuildResultXDR(true)
					require.NoError(t, err)
					return r
				}(),
				ResultMetaXDR: func() string {
					r, err := txbuildtest.BuildOpenResultMetaXDR(txbuildtest.OpenResultMetaParams{
						InitiatorSigner:         localSigner.Address(),
						ResponderSigner:         remoteSigner.Address(),
						InitiatorChannelAccount: localChannelAccount.Address(),
						ResponderChannelAccount: remoteChannelAccount.Address(),
						StartSequence:           28037546508289,
						Asset:                   txnbuild.NativeAsset{},
					})
					require.NoError(t, err)
					return r
				}(),
			}
			go func() {
				localVars.transactionsStream <- streamedTx
				remoteVars.transactionsStream <- streamedTx
			}()
			return nil
		}),
		streamer: streamerFunc(func(cursor string, accounts ...*keypair.FromAddress) (transactions <-chan StreamedTransaction, cancel func()) {
			return localVars.transactionsStream, func() {}
		}),
		channelAccountKey:    localChannelAccount.FromAddress(),
		channelAccountSigner: localSigner,
		logWriter:            io.Discard,
	}

	// Setup the remote agent.
	remoteAgent := &Agent{
		observationPeriodTime:      20 * time.Second,
		observationPeriodLedgerGap: 1,
		maxOpenExpiry:              5 * time.Minute,
		networkPassphrase:          network.TestNetworkPassphrase,
		sequenceNumberCollector: sequenceNumberCollector(func(accountID *keypair.FromAddress) (int64, error) {
			if accountID.Equal(localChannelAccount) {
				return 28037546508288, nil
			}
			if accountID.Equal(remoteChannelAccount) {
				return 28054726377472, nil
			}
			return 0, fmt.Errorf("unknown channel account")
		}),
		balanceCollector: balanceCollectorFunc(func(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
			return 100_0000000, nil
		}),
		submitter: submitterFunc(func(tx *txnbuild.Transaction) error {
			return nil
		}),
		streamer: streamerFunc(func(cursor string, accounts ...*keypair.FromAddress) (transactions <-chan StreamedTransaction, cancel func()) {
			return remoteVars.transactionsStream, func() {}
		}),
		channelAccountKey:    remoteChannelAccount.FromAddress(),
		channelAccountSigner: remoteSigner,
		logWriter:            io.Discard,
	}

	// Connect the two agents.
	type ReadWriter struct {
		io.Reader
		io.Writer
	}
	localReader, localWriter := io.Pipe()
	remoteReader, remoteWriter := io.Pipe()
	localAgent.conn = ReadWriter{
		Reader: remoteReader,
		Writer: localWriter,
	}
	remoteAgent.conn = ReadWriter{
		Reader: localReader,
		Writer: remoteWriter,
	}
	go localAgent.receiveLoop()
	go remoteAgent.receiveLoop()

	localConnected := make(chan struct{})
	localOpened := make(chan struct{})
	localPaymentConfirmedOrError := make(chan struct{})
	localEvents := make(chan interface{}, 2)
	localAgent.events = localEvents
	go func() {
		for {
			e := <-localEvents
			t.Logf("local event: %#v", e)
			switch e.(type) {
			case ConnectedEvent:
				close(localConnected)
			case OpenedEvent:
				close(localOpened)
			case PaymentSentEvent, ErrorEvent:
				close(localPaymentConfirmedOrError)
			}
		}
	}()
	remoteConnected := make(chan struct{})
	remoteOpened := make(chan struct{})
	remotePaymentConfirmedOrError := make(chan struct{})
	remoteEvents := make(chan interface{}, 2)
	remoteAgent.events = remoteEvents
	go func() {
		for {
			e := <-remoteEvents
			t.Logf("remote event: %#v", e)
			switch e.(type) {
			case ConnectedEvent:
				close(remoteConnected)
			case OpenedEvent:
				close(remoteOpened)
			case PaymentReceivedEvent, ErrorEvent:
				close(remotePaymentConfirmedOrError)
			}
		}
	}()

	err := localAgent.hello()
	require.NoError(t, err)
	err = remoteAgent.hello()
	require.NoError(t, err)

	<-localConnected
	<-remoteConnected

	// Open the channel.
	err = localAgent.Open(state.NativeAsset)
	require.NoError(t, err)

	<-localOpened
	<-remoteOpened

	// Make a payment.
	err = localAgent.Payment(50_0000000)
	require.NoError(t, err)
	err = remoteAgent.Payment(50_0000000)
	require.NoError(t, err)

	<-localPaymentConfirmedOrError
	<-remotePaymentConfirmedOrError
}
