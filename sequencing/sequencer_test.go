package sequencing

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/AnomalyFi/go-sequencing"
	"github.com/rollkit/rollkit/types"
)

func TestSequence(t *testing.T) {
	var chainID_test = "2dEiRo5hFTeGZFsUJJKCKidS7Do9zktBeDkEbffMRrnaYoVJ69"
	var uri_test = "http://34.227.74.165:9650/ext/bc/2dEiRo5hFTeGZFsUJJKCKidS7Do9zktBeDkEbffMRrnaYoVJ69"
	var rollupChainID_test = uint64(45200)
	var rollupNamespace_test = make([]byte, 8)
	binary.LittleEndian.PutUint64(rollupNamespace_test, rollupChainID_test)
	fmt.Printf("rollupNamespace_test: %v\n", rollupNamespace_test)
	var nextBatch_test *sequencing.Batch
	var lastBatch_test *sequencing.Batch
	var (
		batchTime_test     time.Duration
		da_address_test    string
		da_namespace_test  string
		da_auth_token_test string
	)
	const (
		defaultHost_test      = "localhost"
		defaultPort_test      = "50051"
		defaultBatchTime_test = time.Duration(2 * time.Second)
		defaultDA_test        = "http://localhost:7980"
	)
	config := SEQConfig {
		chainID: chainID_test,
		uri: uri_test,
	}
	seq := NewSEQClient(config.uri, config.chainID)
	if seq == nil {
		fmt.Println("Failed to create SEQClient")
	}
	da_address_test = "http://localhost:7980"
	da_namespace_test = ""
	da_auth_token_test = ""
	cli, err := NewSequencer(da_address_test, da_auth_token_test, da_namespace_test, batchTime_test, seq)
	if err != nil {
		fmt.Printf("Error initializing Sequencer: %v\n", err)
		return 
	}
	if cli == nil {
		fmt.Printf(" seq is nil after initialization %v\n", cli)
		return
	}
	fmt.Println("Initialized Sequencer")
	ctx := context.Background()
	if ctx == nil {
		fmt.Printf(" ctx is nil %v\n", ctx)
		return
	}
	mpoolTxs := types.Txs{
		types.Tx{1, 2, 3, 4},
		types.Tx{5, 6, 7, 8},
		types.Tx{9, 10, 11, 12},
		types.Tx{13, 14, 15, 16},
		types.Tx{17, 18, 19, 20},
	}
	fmt.Println("Created mempool transactions", mpoolTxs)
	for _,tx := range mpoolTxs {
		// submits mempool transaction(s) to the sequencer
		fmt.Println("rollup ns ", rollupNamespace_test)
		fmt.Println("tx before submission ", tx)
		err := cli.SubmitRollupTransaction(ctx, rollupNamespace_test, tx)
		if err != nil {
			return
		}
		fmt.Println("Submitted transaction")
	}
	time.Sleep(10 * time.Second)
	namespaceStr := hex.EncodeToString(rollupNamespace_test)
	fmt.Printf("Converted namespace: %v\n", namespaceStr)
	lastBatch_test = &sequencing.Batch {
		Transactions: mpoolTxs.ToSliceOfBytes(),
		Height: uint64(9393),
		Namespace: namespaceStr,
	}
	fmt.Printf("lastBatch %v\n", lastBatch_test)
	// add first batch to queue
	bq := NewBatchQueue()
	bq.AddBatch(*lastBatch_test)
	fmt.Println("batch queue", bq)
	// new txs for next batch to return
	newTxs := types.Txs{
		types.Tx{21, 22, 23, 24},
		types.Tx{25, 26, 27, 28},
	}
	fmt.Println("Created new mempool transactions", newTxs)
	time.Sleep(2 * time.Second)
	for _, tx := range newTxs {
		err := cli.SubmitRollupTransaction(ctx, rollupNamespace_test, tx)
		if err != nil {
			return
		}
		fmt.Println("Submitted NEW transaction")
	}
	nextBatch_test, err = cli.GetNextBatch(ctx, lastBatch_test)
	if err != nil {
		fmt.Printf("Error getting next batch: %v\n", err)
		return
	}
	fmt.Printf("nextBatch %v\n", nextBatch_test)
	// verify that nextBatch only includes new transactions
	expectedNextBatch := &sequencing.Batch{
    	Transactions: newTxs.ToSliceOfBytes(),
    	Height:       lastBatch_test.Height,
    	Namespace:    namespaceStr,
	}
	fmt.Printf("expected Batch %v\n", expectedNextBatch)
	if reflect.DeepEqual(nextBatch_test, expectedNextBatch) {
    	fmt.Println("Test passed: nextBatch only includes new transactions")
	} else {
    	fmt.Println("Test failed: nextBatch does not match expected transactions")
}
	// verify the batch we just retrieved
	isValid, err := cli.VerifyBatch(ctx, nextBatch_test)
	if err != nil {
		fmt.Printf("Error verifying batch: %v\n", err)
		return
	}
	if !isValid {
		return
	}
	fmt.Printf("Verify Batch %v\n", isValid)
}