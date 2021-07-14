package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/stellar/experimental-payment-channels/sdk/state"
	"github.com/stellar/experimental-payment-channels/sdk/txbuild"
	"github.com/stellar/go/amount"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/txnbuild"
)

const (
	observationPeriodTime      = 10 * time.Second
	observationPeriodLedgerGap = 1
	openExpiry                 = 30 * time.Second
)

func main() {
	err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
}

func run() error {
	showHelp := false
	horizonURL := "http://localhost:8000"
	accountKeyStr := "G..."
	signerKeyStr := "S..."

	fs := flag.NewFlagSet("console", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.BoolVar(&showHelp, "h", showHelp, "Show this help")
	fs.StringVar(&horizonURL, "horizon-url", horizonURL, "Horizon URL")
	fs.StringVar(&accountKeyStr, "account", accountKeyStr, "Account G address")
	fs.StringVar(&signerKeyStr, "signer", signerKeyStr, "Account S signer")
	err := fs.Parse(os.Args[1:])
	if err != nil {
		return err
	}
	if showHelp {
		fs.Usage()
		return nil
	}
	if accountKeyStr == "" {
		return fmt.Errorf("-account required")
	}
	if signerKeyStr == "" {
		return fmt.Errorf("-signer required")
	}

	accountKey, err := keypair.ParseAddress(accountKeyStr)
	if err != nil {
		return fmt.Errorf("cannot parse -account: %w", err)
	}
	signerKey, err := keypair.ParseFull(signerKeyStr)
	if err != nil {
		return fmt.Errorf("cannot parse -signer: %w", err)
	}

	client := &horizonclient.Client{HorizonURL: horizonURL}
	networkDetails, err := client.Root()
	if err != nil {
		return err
	}

	account, err := client.AccountDetail(horizonclient.AccountRequest{AccountID: accountKey.Address()})
	if horizonclient.IsNotFoundError(err) {
		fmt.Fprintf(os.Stderr, "account %s does not exist, attempting to create using network root key\n", accountKey.Address())
		err = fund(client, networkDetails.NetworkPassphrase, accountKey)
	}
	if err != nil {
		return err
	}
	accountSeqNum, err := account.GetSequenceNumber()
	if err != nil {
		return err
	}

	conn := net.Conn(nil)
	escrowAccountKey := keypair.MustRandom()
	otherEscrowAccountKey := (*keypair.FromAddress)(nil)
	otherSignerKey := (*keypair.FromAddress)(nil)
	channel := (*state.Channel)(nil)

	fmt.Fprintln(os.Stdout, "escrow account:", escrowAccountKey.Address())
	tx, err := txbuild.CreateEscrow(txbuild.CreateEscrowParams{
		Creator:        accountKey.FromAddress(),
		Escrow:         escrowAccountKey.FromAddress(),
		SequenceNumber: accountSeqNum + 1,
		Asset:          txnbuild.NativeAsset{},
	})
	if err != nil {
		return fmt.Errorf("creating escrow account tx: %w", err)
	}
	tx, err = tx.Sign(networkDetails.NetworkPassphrase, signerKey, escrowAccountKey)
	if err != nil {
		return fmt.Errorf("signing tx to create escrow account: %w", err)
	}
	err = SubmitFeeBumpTx(client, networkDetails.NetworkPassphrase, tx, accountKey.FromAddress(), signerKey)
	if err != nil {
		return fmt.Errorf("submitting tx to create escrow account: %w", err)
	}

	br := bufio.NewReader(os.Stdin)
Input:
	for {
		fmt.Fprintf(os.Stdout, "> ")
		line, err := br.ReadString('\n')
		if err == io.EOF {
			fmt.Fprintf(os.Stderr, "connection terminated\n")
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %#v\n", err)
			continue
		}
		params := strings.Fields(line)
		if len(params) == 0 {
			continue
		}
		switch params[0] {
		case "help":
			fmt.Fprintf(os.Stderr, "listen [addr]:<port> - listen for a peer to connect\n")
			fmt.Fprintf(os.Stderr, "connect <addr>:<port> - connect to a peer\n")
			fmt.Fprintf(os.Stderr, "open - open a channel with asset\n")
			fmt.Fprintf(os.Stderr, "deposit <amount> - deposit asset into escrow account\n")
			fmt.Fprintf(os.Stderr, "pay <amount> - pay amount of asset to peer\n")
			fmt.Fprintf(os.Stderr, "close - close the channel\n")
			fmt.Fprintf(os.Stderr, "status - display the channel\n")
			fmt.Fprintf(os.Stderr, "exit - exit the application\n")
		case "status":
			fmt.Fprintf(os.Stderr, "%#v\n", channel)
		case "listen":
			if conn != nil {
				fmt.Fprintf(os.Stderr, "error: already connected to a peer\n")
				continue
			}
			ln, err := net.Listen("tcp", params[1])
			if err != nil {
				return err
			}
			conn, err = ln.Accept()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: accepting incoming conn: %v\n", err)
				continue
			}
			fmt.Fprintf(os.Stderr, "connected to %v\n", conn.RemoteAddr())
		case "connect":
			if conn != nil {
				fmt.Fprintf(os.Stderr, "error: already connected to a peer\n")
				continue
			}
			conn, err = net.Dial("tcp", params[1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(os.Stderr, "connected to %v\n", conn.RemoteAddr())
		case "wait":
			if conn == nil {
				fmt.Fprintf(os.Stderr, "error: not connected to a peer\n")
				continue
			}
			dec := json.NewDecoder(io.TeeReader(conn, io.Discard))
			enc := json.NewEncoder(io.MultiWriter(conn, io.Discard))
			for {
				m := message{}
				err := dec.Decode(&m)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
					continue
				}
				if m.Introduction != nil {
					otherEscrowAccountKey, err = keypair.ParseAddress(m.Introduction.EscrowAccount)
					if err != nil {
						fmt.Fprintf(os.Stderr, "error: parsing other's escrow account: %v\n", err)
						continue
					}
					otherSignerKey, err = keypair.ParseAddress(m.Introduction.Signer)
					if err != nil {
						fmt.Fprintf(os.Stderr, "error: parsing other's signer: %v\n", err)
						continue
					}
					fmt.Fprintf(os.Stdout, "other's signer: %v\n", otherSignerKey.Address())
					fmt.Fprintf(os.Stdout, "other's escrow account: %v\n", otherEscrowAccountKey.Address())
					err = enc.Encode(message{
						Introduction: &introduction{
							EscrowAccount: escrowAccountKey.Address(),
							Signer:        accountKey.Address(),
						},
					})
					if err != nil {
						fmt.Fprintf(os.Stderr, "error: %v\n", err)
						continue
					}
					escrowAccountSeqNum, err := getSeqNum(client, escrowAccountKey.Address())
					if err != nil {
						fmt.Fprintf(os.Stderr, "error: %v\n", err)
						continue
					}
					otherEscrowAccountSeqNum, err := getSeqNum(client, otherEscrowAccountKey.Address())
					if err != nil {
						fmt.Fprintf(os.Stderr, "error: %v\n", err)
						continue
					}
					fmt.Fprintf(os.Stdout, "this's escrow account seq: %v\n", escrowAccountSeqNum)
					fmt.Fprintf(os.Stdout, "other's escrow account seq: %v\n", otherEscrowAccountSeqNum)
					channel = state.NewChannel(state.Config{
						NetworkPassphrase: networkDetails.NetworkPassphrase,
						MaxOpenExpiry:     openExpiry,
						LocalEscrowAccount: &state.EscrowAccount{
							Address:        escrowAccountKey.FromAddress(),
							SequenceNumber: escrowAccountSeqNum,
						},
						RemoteEscrowAccount: &state.EscrowAccount{
							Address:        otherEscrowAccountKey,
							SequenceNumber: otherEscrowAccountSeqNum,
						},
						Initiator:    false,
						LocalSigner:  signerKey,
						RemoteSigner: otherSignerKey,
					})
				} else if m.Open != nil {
					for {
						open, authorized, err := channel.ConfirmOpen(*m.Open)
						if err != nil {
							fmt.Fprintf(os.Stderr, "error: confirming open: %v\n", err)
							continue Input
						}
						err = enc.Encode(message{Open: &open})
						if err != nil {
							fmt.Fprintf(os.Stderr, "error: encoding open to send back: %v\n", err)
							continue Input
						}
						if authorized {
							break
						}
						err = dec.Decode(&m)
						if err != nil {
							fmt.Fprintf(os.Stderr, "error: decoding response: %v\n", err)
							continue Input
						}
					}
					break
				} else if m.Update != nil {
					var authorized bool
					for {
						var update state.CloseAgreement
						update, authorized, err = channel.ConfirmPayment(*m.Update)
						if errors.Is(err, state.ErrUnderfunded) {
							fmt.Fprintf(os.Stderr, "remote is underfunded for this payment based on cached account balances, checking their escrow account...\n")
							var account horizon.Account
							account, err = client.AccountDetail(horizonclient.AccountRequest{AccountID: otherEscrowAccountKey.Address()})
							if err != nil {
								return fmt.Errorf("getting state of remote escrow account: %w", err)
							}
							balance, err := amount.ParseInt64(account.Balances[0].Balance)
							if err != nil {
								return fmt.Errorf("parsing balance of remote escrow account: %w", err)
							}
							fmt.Fprintf(os.Stderr, "updating remote escrow balance to: %d\n", balance)
							channel.UpdateRemoteEscrowAccountBalance(balance)
							update, authorized, err = channel.ConfirmPayment(*m.Update)
							if err != nil {
								fmt.Fprintf(os.Stderr, "error: confirming payment: %v\n", err)
								break
							}
						}
						if err != nil {
							fmt.Fprintf(os.Stderr, "error: confirming payment: %v\n", err)
							break
						}
						if authorized {
							break
						}
						err = enc.Encode(message{Update: &update})
						if err != nil {
							fmt.Fprintf(os.Stderr, "error: encoding payment to send back: %v\n", err)
							break
						}
						err = dec.Decode(&m)
						if err != nil {
							fmt.Fprintf(os.Stderr, "error: decoding response: %v\n", err)
							break
						}
					}
					if authorized {
						fmt.Fprintln(os.Stderr, "payment successfully received")
					}
					break
				} else if m.Close != nil {
					close, authorized, err := channel.ConfirmClose(*m.Close)
					if err != nil {
						fmt.Fprintf(os.Stderr, "error: confirming close: %v\n", err)
						break
					}
					err = enc.Encode(message{Close: &close})
					if err != nil {
						fmt.Fprintf(os.Stderr, "error: encoding close to send back: %v\n", err)
						break
					}
					if authorized {
						fmt.Fprintln(os.Stderr, "close ready")
					}
					break
				}
			}
		case "open":
			if conn == nil {
				fmt.Fprintf(os.Stderr, "error: not connected to a peer\n")
				continue
			}
			enc := json.NewEncoder(io.MultiWriter(conn, io.Discard))
			dec := json.NewDecoder(io.TeeReader(conn, io.Discard))
			err = enc.Encode(message{
				Introduction: &introduction{
					EscrowAccount: escrowAccountKey.Address(),
					Signer:        accountKey.Address(),
				},
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			m := message{}
			err := dec.Decode(&m)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			if m.Introduction != nil {
				otherSignerKey, err = keypair.ParseAddress(m.Introduction.Signer)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: parsing other's signer: %v\n", err)
					continue
				}
				otherEscrowAccountKey, err = keypair.ParseAddress(m.Introduction.EscrowAccount)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: parsing other's escrow account: %v\n", err)
					continue
				}
			} else {
				fmt.Fprintf(os.Stderr, "error: unexpected response: %v\n", err)
				continue
			}
			fmt.Fprintf(os.Stdout, "other's signer: %v\n", otherSignerKey.Address())
			fmt.Fprintf(os.Stdout, "other's escrow account: %v\n", otherEscrowAccountKey.Address())
			escrowAccountSeqNum, err := getSeqNum(client, escrowAccountKey.Address())
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			otherEscrowAccountSeqNum, err := getSeqNum(client, otherEscrowAccountKey.Address())
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(os.Stdout, "this's escrow account seq: %v\n", escrowAccountSeqNum)
			fmt.Fprintf(os.Stdout, "other's escrow account seq: %v\n", otherEscrowAccountSeqNum)
			channel = state.NewChannel(state.Config{
				NetworkPassphrase: networkDetails.NetworkPassphrase,
				MaxOpenExpiry:     openExpiry,
				LocalEscrowAccount: &state.EscrowAccount{
					Address:        escrowAccountKey.FromAddress(),
					SequenceNumber: escrowAccountSeqNum,
				},
				RemoteEscrowAccount: &state.EscrowAccount{
					Address:        otherEscrowAccountKey,
					SequenceNumber: otherEscrowAccountSeqNum,
				},
				Initiator:    true,
				LocalSigner:  signerKey,
				RemoteSigner: otherSignerKey,
			})
			openAgreement, err := channel.ProposeOpen(state.OpenParams{
				ObservationPeriodTime:      observationPeriodTime,
				ObservationPeriodLedgerGap: observationPeriodLedgerGap,
				Asset:                      state.NativeAsset,
				ExpiresAt:                  time.Now().Add(openExpiry),
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: proposing open agreement: %v\n", err)
				continue
			}
			err = enc.Encode(message{Open: &openAgreement})
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			for {
				err = dec.Decode(&m)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: decoding response: %v\n", err)
					continue
				}
				open, authorized, err := channel.ConfirmOpen(*m.Open)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: confirming open: %v\n", err)
					continue
				}
				if authorized {
					break
				}
				err = enc.Encode(message{Open: &open})
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: encoding open to send back: %v\n", err)
					continue
				}
			}
			err = SubmitFormationTx(channel, client, networkDetails.NetworkPassphrase, accountKey.FromAddress(), signerKey)
			if err != nil {
				return fmt.Errorf("submitting tx to form the channel: %w", err)
			}
		case "deposit":
			depositAmountStr := params[1]
			depositAmountInt, err := amount.ParseInt64(depositAmountStr)
			if err != nil {
				return fmt.Errorf("parsing deposit amount: %w", err)
			}
			account, err := client.AccountDetail(horizonclient.AccountRequest{AccountID: accountKey.Address()})
			if err != nil {
				return fmt.Errorf("getting state of local escrow account: %w", err)
			}
			tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
				SourceAccount:        &account,
				IncrementSequenceNum: true,
				BaseFee:              txnbuild.MinBaseFee,
				Timebounds:           txnbuild.NewTimeout(300),
				Operations: []txnbuild.Operation{
					&txnbuild.Payment{Destination: escrowAccountKey.Address(), Asset: txnbuild.NativeAsset{}, Amount: depositAmountStr},
				},
			})
			if err != nil {
				return fmt.Errorf("building deposit payment tx: %w", err)
			}
			tx, err = tx.Sign(networkDetails.NetworkPassphrase, signerKey)
			if err != nil {
				return fmt.Errorf("signing deposit payment tx: %w", err)
			}
			_, err = client.SubmitTransaction(tx)
			if err != nil {
				return fmt.Errorf("submitting deposit payment tx: %w", err)
			}
			newBalance := channel.LocalEscrowAccountBalance() + depositAmountInt
			channel.UpdateLocalEscrowAccountBalance(newBalance)
			fmt.Println("new balance of", newBalance)
		case "pay":
			if conn == nil || channel == nil {
				fmt.Fprintf(os.Stderr, "error: not connected to a peer\n")
				continue
			}
			amountValue, err := amount.ParseInt64(params[1])
			if err != nil {
				return fmt.Errorf("parsing the amount: %w", err)
			}
			ca, err := channel.ProposePayment(amountValue)
			if err != nil {
				return fmt.Errorf("proposing the payment: %w", err)
			}
			enc := json.NewEncoder(io.MultiWriter(conn, io.Discard))
			dec := json.NewDecoder(io.TeeReader(conn, io.Discard))
			err = enc.Encode(message{Update: &ca})
			if err != nil {
				return fmt.Errorf("sending the payment: %w", err)
			}
			var authorized bool
			for {
				m := message{}
				err = dec.Decode(&m)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: decoding response: %v\n", err)
					break
				}
				var update state.CloseAgreement
				update, authorized, err = channel.ConfirmPayment(*m.Update)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: confirming payment: %v\n", err)
					break
				}
				err = enc.Encode(message{Update: &update})
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: encoding payment to send back: %v\n", err)
					break
				}
				if authorized {
					break
				}
			}
			if authorized {
				fmt.Fprintln(os.Stderr, "payment successfully sent")
			}
		case "close":
			if conn == nil || channel == nil {
				fmt.Fprintf(os.Stderr, "error: not connected to a peer\n")
				continue
			}
			// Submit declaration tx
			err = SubmitDeclarationTx(channel, client, networkDetails.NetworkPassphrase, accountKey.FromAddress(), signerKey)
			if err != nil {
				return fmt.Errorf("submitting tx to decl the channel: %w", err)
			}
			// Revising agreement to close early
			ca, err := channel.ProposeClose()
			if err != nil {
				return fmt.Errorf("proposing the close: %w", err)
			}
			enc := json.NewEncoder(io.MultiWriter(conn, os.Stdout))
			dec := json.NewDecoder(io.TeeReader(conn, os.Stdout))
			err = enc.Encode(message{Close: &ca})
			if err != nil {
				return fmt.Errorf("sending the payment: %w", err)
			}
			err = conn.SetReadDeadline(time.Now().Add(observationPeriodTime))
			if err != nil {
				return fmt.Errorf("setting read deadline of conn: %w", err)
			}
			timerStart := time.Now()
			authorized := false
			m := message{}
			err = dec.Decode(&m)
			if errors.Is(err, os.ErrDeadlineExceeded) {
			} else {
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: decoding response: %v\n", err)
					break
				}
				_, authorized, err = channel.ConfirmClose(*m.Close)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: confirming close: %v\n", err)
					break
				}
			}
			if authorized {
				fmt.Fprintln(os.Stderr, "close ready")
			} else {
				fmt.Fprintf(os.Stderr, "close not authorized, waiting observation period then closing...")
				time.Sleep(observationPeriodTime*2 - time.Since(timerStart))
			}
			err = SubmitCloseTx(channel, client, networkDetails.NetworkPassphrase, accountKey.FromAddress(), signerKey)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: submitting close: %v\n", err)
				break
			}
		case "exit":
			return nil
		default:
			fmt.Fprintf(os.Stderr, "error: unrecognized command\n")
		}
	}
	return nil
}
