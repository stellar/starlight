package agent

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stellar/experimental-payment-channels/sdk/state"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/txnbuild"
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

type transactionStreamerFunc func(accounts []*keypair.FromAddress, transactions chan<- TransactionStreamed)

func (f transactionStreamerFunc) StreamTransactions(accounts []*keypair.FromAddress, transactions chan<- TransactionStreamed) {
	f(accounts, transactions)
}

func TestAgent_openPaymentClose(t *testing.T) {
	localEscrow := keypair.MustParseAddress("GAU4CFXQI6HLK5PPY2JWU3GMRJIIQNLF24XRAHX235F7QTG6BEKLGQ36")
	localSigner := keypair.MustParseFull("SCBMAMOPWKL2YHWELK63VLAY2R74A6GTLLD4ON223B7K5KZ37MUR6IDF")
	remoteEscrow := keypair.MustParseAddress("GBQNGSEHTFC4YGQ3EXHIL7JQBA6265LFANKFFAYKHM7JFGU5CORROEGO")
	remoteSigner := keypair.MustParseFull("SBM7D2IIDSRX5Y3VMTMTXXPB6AIB4WYGZBC2M64U742BNOK32X6SW4NF")

	// Setup the local agent.
	localVars := struct {
		submittedTx        *txnbuild.Transaction
		transactionsStream chan<- TransactionStreamed
	}{}
	localEvents := make(chan Event, 1)
	localAgent := &Agent{
		ObservationPeriodTime:      20 * time.Second,
		ObservationPeriodLedgerGap: 1,
		MaxOpenExpiry:              5 * time.Minute,
		NetworkPassphrase:          network.TestNetworkPassphrase,
		SequenceNumberCollector: sequenceNumberCollector(func(accountID *keypair.FromAddress) (int64, error) {
			if accountID.Equal(localEscrow) {
				return 28037546508288, nil
			}
			if accountID.Equal(remoteEscrow) {
				return 28054726377472, nil
			}
			return 0, fmt.Errorf("unknown escrow account")
		}),
		BalanceCollector: balanceCollectorFunc(func(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
			return 100_0000000, nil
		}),
		Submitter: submitterFunc(func(tx *txnbuild.Transaction) error {
			localVars.submittedTx = tx
			return nil
		}),
		TransactionStreamer: transactionStreamerFunc(func(accounts []*keypair.FromAddress, transactions chan<- TransactionStreamed) {
			localVars.transactionsStream = transactions
		}),
		EscrowAccountKey:    localEscrow.FromAddress(),
		EscrowAccountSigner: localSigner,
		LogWriter:           io.Discard,
		Events:              localEvents,
	}

	// Setup the remote agent.
	remoteVars := struct {
		submittedTx        *txnbuild.Transaction
		transactionsStream chan<- TransactionStreamed
	}{}
	remoteEvents := make(chan Event, 1)
	remoteAgent := &Agent{
		ObservationPeriodTime:      20 * time.Second,
		ObservationPeriodLedgerGap: 1,
		MaxOpenExpiry:              5 * time.Minute,
		NetworkPassphrase:          network.TestNetworkPassphrase,
		SequenceNumberCollector: sequenceNumberCollector(func(accountID *keypair.FromAddress) (int64, error) {
			if accountID.Equal(localEscrow) {
				return 28037546508288, nil
			}
			if accountID.Equal(remoteEscrow) {
				return 28054726377472, nil
			}
			return 0, fmt.Errorf("unknown escrow account")
		}),
		BalanceCollector: balanceCollectorFunc(func(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
			return 100_0000000, nil
		}),
		Submitter: submitterFunc(func(tx *txnbuild.Transaction) error {
			remoteVars.submittedTx = tx
			return nil
		}),
		TransactionStreamer: transactionStreamerFunc(func(accounts []*keypair.FromAddress, transactions chan<- TransactionStreamed) {
			remoteVars.transactionsStream = transactions
		}),
		EscrowAccountKey:    remoteEscrow.FromAddress(),
		EscrowAccountSigner: remoteSigner,
		LogWriter:           io.Discard,
		Events:              remoteEvents,
	}

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
		assert.Equal(t, localEvent, ConnectedEvent{})
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		assert.Equal(t, remoteEvent, ConnectedEvent{})
	}
	// Open the channel.
	err = localAgent.Open()
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
	openTxStreamed := TransactionStreamed{
		TransactionXDR: openTxXDR,
		ResultXDR:      "AAAAAAAAAGQAAAAAAAAAAQAAAAAAAAABAAAAAAAAAAA=",
		ResultMetaXDR:  "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",
	}
	localVars.transactionsStream <- openTxStreamed
	remoteVars.transactionsStream <- openTxStreamed

	// Expect opened event.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		assert.Equal(t, localEvent, OpenedEvent{})
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		assert.Equal(t, remoteEvent, OpenedEvent{})
	}

	// Make a payment.
	err = localAgent.Payment("50.0")
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.NoError(t, err)
	err = localAgent.receive()
	require.NoError(t, err)

	// Expect payment events.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		localPaymentEvent, ok := localEvent.(PaymentSentAndConfirmedEvent)
		require.True(t, ok)
		assert.Equal(t, int64(2), localPaymentEvent.CloseAgreement.Details.IterationNumber)
		assert.Equal(t, int64(50_0000000), localPaymentEvent.CloseAgreement.Details.Balance)
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		remotePaymentEvent, ok := remoteEvent.(PaymentReceivedAndConfirmedEvent)
		require.True(t, ok)
		assert.Equal(t, int64(2), remotePaymentEvent.CloseAgreement.Details.IterationNumber)
		assert.Equal(t, int64(50_0000000), remotePaymentEvent.CloseAgreement.Details.Balance)
	}

	// Make another payment.
	err = remoteAgent.Payment("20.0")
	require.NoError(t, err)
	err = localAgent.receive()
	require.NoError(t, err)
	err = remoteAgent.receive()
	require.NoError(t, err)

	// Expect payment events.
	{
		localEvent, ok := <-localEvents
		require.True(t, ok)
		localPaymentEvent, ok := localEvent.(PaymentReceivedAndConfirmedEvent)
		require.True(t, ok)
		assert.Equal(t, int64(3), localPaymentEvent.CloseAgreement.Details.IterationNumber)
		assert.Equal(t, int64(30_0000000), localPaymentEvent.CloseAgreement.Details.Balance)
		remoteEvent, ok := <-remoteEvents
		require.True(t, ok)
		remotePaymentEvent, ok := remoteEvent.(PaymentSentAndConfirmedEvent)
		require.True(t, ok)
		assert.Equal(t, int64(3), remotePaymentEvent.CloseAgreement.Details.IterationNumber)
		assert.Equal(t, int64(30_0000000), remotePaymentEvent.CloseAgreement.Details.Balance)
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
	localDeclTxStreamed := TransactionStreamed{
		TransactionXDR: localDeclTxXDR,
		ResultXDR:      "AAAAAAAAAGQAAAAAAAAAAQAAAAAAAAABAAAAAAAAAAA=",
		ResultMetaXDR:  "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",
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
	localCloseTxStreamed := TransactionStreamed{
		TransactionXDR: localCloseTxXDR,
		ResultXDR:      "AAAAAAAAAGQAAAAAAAAAAQAAAAAAAAABAAAAAAAAAAA=",
		ResultMetaXDR:  "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",
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
	localEscrow := keypair.MustRandom()
	localSigner := keypair.MustRandom()
	remoteEscrow := keypair.MustRandom()
	remoteSigner := keypair.MustRandom()

	// Setup the local agent.
	localVars := struct {
		submittedTx *txnbuild.Transaction
	}{}
	localAgent := &Agent{
		ObservationPeriodTime:      20 * time.Second,
		ObservationPeriodLedgerGap: 1,
		MaxOpenExpiry:              5 * time.Minute,
		NetworkPassphrase:          network.TestNetworkPassphrase,
		SequenceNumberCollector: sequenceNumberCollector(func(accountID *keypair.FromAddress) (int64, error) {
			return 1, nil
		}),
		BalanceCollector: balanceCollectorFunc(func(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
			return 100_0000000, nil
		}),
		Submitter: submitterFunc(func(tx *txnbuild.Transaction) error {
			localVars.submittedTx = tx
			return nil
		}),
		EscrowAccountKey:    localEscrow.FromAddress(),
		EscrowAccountSigner: localSigner,
		LogWriter:           io.Discard,
	}

	// Setup the remote agent.
	remoteVars := struct {
		submittedTx *txnbuild.Transaction
	}{}
	remoteAgent := &Agent{
		ObservationPeriodTime:      20 * time.Second,
		ObservationPeriodLedgerGap: 1,
		MaxOpenExpiry:              5 * time.Minute,
		NetworkPassphrase:          network.TestNetworkPassphrase,
		SequenceNumberCollector: sequenceNumberCollector(func(accountID *keypair.FromAddress) (int64, error) {
			return 1, nil
		}),
		BalanceCollector: balanceCollectorFunc(func(accountID *keypair.FromAddress, asset state.Asset) (int64, error) {
			return 100_0000000, nil
		}),
		Submitter: submitterFunc(func(tx *txnbuild.Transaction) error {
			remoteVars.submittedTx = tx
			return nil
		}),
		EscrowAccountKey:    remoteEscrow.FromAddress(),
		EscrowAccountSigner: remoteSigner,
		LogWriter:           io.Discard,
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
	localEvents := make(chan Event, 2)
	localAgent.Events = localEvents
	go func() {
		for {
			e := <-localEvents
			t.Logf("local event: %#v", e)
			switch e.(type) {
			case ConnectedEvent:
				close(localConnected)
			case OpenedEvent:
				close(localOpened)
			case PaymentSentAndConfirmedEvent, ErrorEvent:
				close(localPaymentConfirmedOrError)
			}
		}
	}()
	remoteConnected := make(chan struct{})
	remoteOpened := make(chan struct{})
	remotePaymentConfirmedOrError := make(chan struct{})
	remoteEvents := make(chan Event, 2)
	remoteAgent.Events = remoteEvents
	go func() {
		for {
			e := <-remoteEvents
			t.Logf("remote event: %#v", e)
			switch e.(type) {
			case ConnectedEvent:
				close(remoteConnected)
			case OpenedEvent:
				close(remoteOpened)
			case PaymentReceivedAndConfirmedEvent, ErrorEvent:
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
	err = localAgent.Open()
	require.NoError(t, err)

	<-localOpened
	<-remoteOpened

	// Make a payment.
	err = localAgent.Payment("50.0")
	require.NoError(t, err)
	err = remoteAgent.Payment("50.0")
	require.NoError(t, err)

	<-localPaymentConfirmedOrError
	<-remotePaymentConfirmedOrError
}
