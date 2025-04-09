package paypurse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	feemodel "github.com/bsv-blockchain/go-sdk/transaction/fee_model"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/redis/go-redis/v9"
)

const SATS_PER_KB = uint64(10)

type payPurse struct {
	pk            *ec.PrivateKey
	db            *redis.Client
	SatsPerKb     uint64
	Address       *script.Address
	lockingScript *script.Script
	unlock        *p2pkh.P2PKH
	ChangeSplits  uint8
}

func NewPayPurse(connString, wif string) (p *payPurse, err error) {
	p = &payPurse{}
	if p.pk, err = ec.PrivateKeyFromWif(wif); err != nil {
		return nil, err
	} else if p.Address, err = script.NewAddressFromPublicKey(p.pk.PubKey(), true); err != nil {
		return nil, err
	} else if p.lockingScript, err = p2pkh.Lock(p.Address); err != nil {
		return nil, err
	} else if p.unlock, err = p2pkh.Unlock(p.pk, nil); err != nil {
		return nil, err
	} else if opts, err := redis.ParseURL(connString); err != nil {
		return nil, err
	} else {
		p.db = redis.NewClient(opts)
	}
	return
}

func (p *payPurse) UpdateFromTx(ctx context.Context, tx *transaction.Transaction) error {
	for _, txin := range tx.Inputs {
		outpoint := &overlay.Outpoint{
			Txid:        *txin.SourceTXID,
			OutputIndex: txin.SourceTxOutIndex,
		}
		if err := p.db.ZRem(ctx, "u:"+p.Address.AddressString, outpoint.String()).Err(); err != nil {
			return err
		}
	}
	txid := tx.TxID()
	for vout, txout := range tx.Outputs {
		outpoint := &overlay.Outpoint{
			Txid:        *txid,
			OutputIndex: uint32(vout),
		}
		if bytes.Equal(*txout.LockingScript, *p.lockingScript) {
			if err := p.db.ZAdd(ctx, "u:"+p.Address.AddressString, redis.Z{
				Score:  float64(txout.Satoshis),
				Member: outpoint.String(),
			}).Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *payPurse) FundAndSign(ctx context.Context, tx *transaction.Transaction, fundOutputs bool) error {
	feeModel := &feemodel.SatoshisPerKilobyte{Satoshis: p.SatsPerKb}
	satsIn := uint64(0)
	for _, txin := range tx.Inputs {
		sourceOutput := txin.SourceTxOutput()
		if sourceOutput == nil {
			return transaction.ErrEmptyPreviousTx
		}
		satsIn += sourceOutput.Satoshis
	}
	satsOut := uint64(0)
	for _, txout := range tx.Outputs {
		satsOut += txout.Satoshis
	}
	if fundOutputs && satsIn < satsOut {
		return transaction.ErrInsufficientInputs
	}
	fee, err := feeModel.ComputeFee(tx)
	if err != nil {
		log.Panicln(err)
	}
	for satsIn < satsOut+fee {
		if utxos, err := p.LockUtxos(ctx, satsOut+fee+10); err != nil {
			log.Panicln(err)
		} else {
			for _, u := range utxos {
				tx.AddInputsFromUTXOs()
				satsIn += u.Satoshis
			}
		}
	}
	for range p.ChangeSplits {
		tx.AddOutput(&transaction.TransactionOutput{
			LockingScript: p.lockingScript,
			Change:        true,
		})
	}
	if err := tx.Fee(feeModel, transaction.ChangeDistributionEqual); err != nil {
		log.Println(err)
		return err
	} else if err := tx.Sign(); err != nil {
		log.Println(err)
		return err
	}
	return nil
}

func (p *payPurse) LockUtxos(ctx context.Context, satoshis uint64) ([]*transaction.UTXO, error) {
	results, err := p.db.ZRangeArgsWithScores(ctx, redis.ZRangeArgs{
		Key:     "u:" + p.Address.AddressString,
		ByScore: true,
		Rev:     true,
		Count:   25,
	}).Result()
	if err != nil {
		log.Println(err)
		return nil, err
	}

	utxos := make([]*transaction.UTXO, 0, len(results))
	collected := uint64(0)
	for _, result := range results {
		if collected >= satoshis {
			break
		}
		op := result.Member.(string)
		if outpoint, err := overlay.NewOutpointFromString(op); err != nil {
			log.Panicln(err)
		} else if locked, err := p.db.SetNX(ctx, "lock:"+op, time.Now().Unix(), time.Minute).Result(); err != nil {
			log.Panic(err)
		} else if locked {
			utxos = append(utxos, &transaction.UTXO{
				TxID:                    &outpoint.Txid,
				Vout:                    outpoint.OutputIndex,
				LockingScript:           p.lockingScript,
				Satoshis:                uint64(result.Score),
				UnlockingScriptTemplate: p.unlock,
			})
			collected += uint64(result.Score)
		}
	}
	if collected < satoshis {
		return nil, transaction.ErrInsufficientFunds
	}
	return utxos, nil
}

type WOCResponse struct {
	Error  string    `json:"error"`
	Result []WOCUtxo `json:"result"`
}

type WOCUtxo struct {
	TxPos   uint32 `json:"tx_pos"`
	TxHash  string `json:"tx_hash"`
	Value   uint64 `json:"value"`
	IsSpent bool   `json:"isSpentInMempoolTx"`
	Status  string `json:"status"`
}

func (p *payPurse) Balance(ctx context.Context) (bal uint64, count int, err error) {
	if results, err := p.db.ZRangeArgsWithScores(ctx, redis.ZRangeArgs{
		Key:     "u:" + p.Address.AddressString,
		ByScore: true,
		Rev:     true,
		Count:   25,
	}).Result(); err == nil {
		for _, result := range results {
			bal += uint64(result.Score)
			count++
		}
	}
	return
}
func (p *payPurse) RefreshBalance(ctx context.Context, tx *transaction.Transaction) error {
	if resp, err := http.Get("https://api.whatsonchain.com/v1/bsv/main/address/" + p.Address.AddressString + "/unspent/all"); err != nil {
		log.Println(err)
		return err
	} else if resp.StatusCode != http.StatusOK {
		log.Println("Error: ", resp.Status)
		return transaction.ErrEmptyPreviousTx
	} else if resp.Body != nil {
		defer resp.Body.Close()
		var response WOCResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			log.Println(err)
			return err
		}
		if p.db.Del(ctx, "u:"+p.Address.AddressString).Err() != nil {
			log.Println(err)
		}
		for _, u := range response.Result {
			if u.IsSpent {
				continue
			} else if err := p.db.ZAdd(ctx, "u:"+p.Address.AddressString, redis.Z{
				Score:  float64(u.Value),
				Member: fmt.Sprintf("%s.%d", u.TxHash, u.TxPos),
			}).Err(); err != nil {
				log.Println(err)
			}
		}
	}
	return nil
}
