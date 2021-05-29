package state

import (
	"time"

	"github.com/pkg/errors"

	"github.com/stellar/experimental-payment-channels/sdk/txbuild"
	"github.com/stellar/go/xdr"
)

type PaymentProposal struct {
	IterationNumber            int64
	ObservationPeriodTime      time.Duration
	ObservationPeriodLedgerGap int64
	AmountToInitiator          int64
	AmountToResponder          int64
	CloseSignatures            []xdr.DecoratedSignature
	DeclarationSignatures      []xdr.DecoratedSignature
}

// TODO - validate inputs? (eg. no negative amounts)
// TODO - payments to be in Amount struct
// initiator will only call this
func (c *Channel) NewPaymentProposal(payToInitiator int64, payToResponder int64) (*PaymentProposal, error) {
	newBalance := c.Balance + payToResponder - payToInitiator
	amountToInitiator := int64(0)
	amountToResponder := int64(0)
	if newBalance > 0 {
		amountToResponder = newBalance
	} else {
		amountToInitiator = newBalance * -1
	}
	txC, err := txbuild.Close(txbuild.CloseParams{
		ObservationPeriodTime:      c.observationPeriodTime,
		ObservationPeriodLedgerGap: c.observationPeriodLedgerGap,
		InitiatorSigner:            c.localSigner.FromAddress(),
		ResponderSigner:            c.remoteSigner,
		InitiatorEscrow:            c.localEscrowAccount.Address,
		ResponderEscrow:            c.remoteEscrowAccount.Address,
		StartSequence:              c.startingSequence,
		IterationNumber:            c.iterationNumber,
		AmountToInitiator:          amountToInitiator,
		AmountToResponder:          amountToResponder,
	})
	if err != nil {
		return nil, err
	}
	txC, err = txC.Sign(c.networkPassphrase, c.localSigner)
	if err != nil {
		return nil, err
	}
	c.Balance = newBalance
	return &PaymentProposal{
		ObservationPeriodTime:      c.observationPeriodTime,
		ObservationPeriodLedgerGap: c.observationPeriodLedgerGap,
		AmountToInitiator:          payToInitiator,
		AmountToResponder:          payToResponder,
		CloseSignatures:            txC.Signatures(),
	}, nil
}

func (c *Channel) ConfirmPayment(p *PaymentProposal) (*PaymentProposal, error) {
	if !c.initiator {
		newBalance := c.Balance + p.AmountToResponder - p.AmountToInitiator
		// TODO - better var names to differentiate from C_i fields?
		amountToInitiator := int64(0)
		amountToResponder := int64(0)
		if newBalance > 0 {
			amountToResponder = newBalance
		} else {
			amountToInitiator = newBalance * -1
		}
		txC, err := txbuild.Close(txbuild.CloseParams{
			ObservationPeriodTime:      c.observationPeriodTime,
			ObservationPeriodLedgerGap: c.observationPeriodLedgerGap,
			InitiatorSigner:            c.remoteSigner,
			ResponderSigner:            c.localSigner.FromAddress(),
			InitiatorEscrow:            c.remoteEscrowAccount.Address,
			ResponderEscrow:            c.localEscrowAccount.Address,
			StartSequence:              c.startingSequence,
			IterationNumber:            c.iterationNumber,
			AmountToInitiator:          amountToInitiator,
			AmountToResponder:          amountToResponder,
		})
		if err != nil {
			return nil, err
		}
		if err := c.verifySigned(txC, p.CloseSignatures, c.remoteSigner); err != nil {
			return nil, errors.Wrap(err, "the signed transaction may have different data")
		}
		// TODO - why is signing here bad?
		txC, err = txC.Sign(c.networkPassphrase, c.localSigner)
		if err != nil {
			return nil, err
		}
		txD, err := txbuild.Declaration(txbuild.DeclarationParams{
			InitiatorEscrow:         c.remoteEscrowAccount.Address,
			StartSequence:           c.startingSequence,
			IterationNumber:         c.iterationNumber,
			IterationNumberExecuted: c.iterationNumberExecuted,
		})
		if err != nil {
			return nil, err
		}
		txD, err = txD.Sign(c.networkPassphrase, c.localSigner)
		if err != nil {
			return nil, err
		}
		c.Balance = newBalance
		p.CloseSignatures = append(p.CloseSignatures, txC.Signatures()...)
		p.DeclarationSignatures = append(p.DeclarationSignatures, txD.Signatures()...)
		return p, nil
	}
	// TODO - split up function better
	txD, err := txbuild.Declaration(txbuild.DeclarationParams{
		InitiatorEscrow:         c.localEscrowAccount.Address,
		StartSequence:           c.startingSequence,
		IterationNumber:         c.iterationNumber,
		IterationNumberExecuted: c.iterationNumberExecuted,
	})
	if err != nil {
		return nil, err
	}
	// TODO - add sign verification here as well?
	txD, err = txD.Sign(c.networkPassphrase, c.localSigner)
	if err != nil {
		return nil, err
	}

	p.DeclarationSignatures = append(p.DeclarationSignatures, txD.Signatures()...)
	return p, nil
}
