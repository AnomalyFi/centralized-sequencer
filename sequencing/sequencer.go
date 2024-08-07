package sequencing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	logging "github.com/ipfs/go-log/v2"

	"github.com/AnomalyFi/centralized-sequencer/da"
	goda "github.com/rollkit/go-da"
	proxyda "github.com/rollkit/go-da/proxy"

	"github.com/AnomalyFi/go-sequencing"
	conf "github.com/AnomalyFi/nodekit-relay/config"
	relay "github.com/AnomalyFi/nodekit-relay/rpc"
	trpc "github.com/AnomalyFi/seq-sdk/client"
	info "github.com/AnomalyFi/seq-sdk/types"
)

var _ sequencing.Sequencer = &Sequencer{}
var log = logging.Logger("centralized-sequencer")

const maxSubmitAttempts = 30
const defaultMempoolTTL = 25

var initialBackoff = 100 * time.Millisecond

// ErrorRollupIdMismatch is returned when the rollup id does not match
var ErrorRollupIdMismatch = errors.New("rollup id mismatch")

// NodeKit Vars
var chainID = "2dEiRo5hFTeGZFsUJJKCKidS7Do9zktBeDkEbffMRrnaYoVJ69"
var uri = "http://34.227.74.165:9650/ext/bc/2dEiRo5hFTeGZFsUJJKCKidS7Do9zktBeDkEbffMRrnaYoVJ69"

// each rollup will have a specific chain ID
// below is how the chain id and namespace of rollup are stored into a unique byte array
// important for fetching txs by namespace and height
var rollupChainID = uint64(45200)
var rollupNamespace []byte
var cli = NewSEQClient(uri, chainID)
var blockHeight = uint64(0)

type SEQConfig struct {
	chainID string
	uri     string
}
// NodeKit Client
type SEQClient struct {
	seqClient *trpc.JSONRPCClient
}

func NewSEQClient(url string, id string) *SEQClient {
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}

	cli := trpc.NewJSONRPCClient(url, 12345, id)

	return &SEQClient{
		seqClient: cli,
	}
}

// BatchQueue ...
type BatchQueue struct {
	queue []sequencing.Batch
	mu    sync.Mutex
}

// NewBatchQueue creates a new TransactionQueue
func NewBatchQueue() *BatchQueue {
	return &BatchQueue{
		queue: make([]sequencing.Batch, 0),
	}
}

// AddBatch adds a new transaction to the queue
func (bq *BatchQueue) AddBatch(batch sequencing.Batch) {
	bq.mu.Lock()
	defer bq.mu.Unlock()
	bq.queue = append(bq.queue, batch)
}

// Next ...
func (bq *BatchQueue) Next() *sequencing.Batch {
	if len(bq.queue) == 0 {
		return nil
	}
	batch := bq.queue[0]
	bq.queue = bq.queue[1:]
	return &batch
}

// TransactionQueue is a queue of transactions
type TransactionQueue struct {
	queue []sequencing.Tx
	mu    sync.Mutex
}

// NewTransactionQueue creates a new TransactionQueue
func NewTransactionQueue() *TransactionQueue {
	return &TransactionQueue{
		queue: make([]sequencing.Tx, 0),
	}
}

// AddTransaction adds a new transaction to the queue
func (tq *TransactionQueue) AddTransaction(tx sequencing.Tx) {
	tq.mu.Lock()
	defer tq.mu.Unlock()
	tq.queue = append(tq.queue, tx)
}

// GetNextBatch extracts a batch of transactions from the queue
func (tq *TransactionQueue) GetNextBatch(max uint64) sequencing.Batch {
	tq.mu.Lock()
	defer tq.mu.Unlock()

	var batch [][]byte
	batchSize := len(tq.queue)
	for {
		batch = tq.queue[:batchSize]
		blobSize := totalBytes(batch)
		if uint64(blobSize) < max {
			break
		}
		batchSize = batchSize - 1
	}

	tq.queue = tq.queue[batchSize:]
	return sequencing.Batch{Transactions: batch}
}

func totalBytes(data [][]byte) int {
	total := 0
	for _, sub := range data {
		total += len(sub)
	}
	return total
}

// Sequencer implements go-sequencing interface using celestia backend
type Sequencer struct {
	dalc          *da.DAClient
	batchTime     time.Duration
	ctx           context.Context
	maxDABlobSize uint64

	rollupId sequencing.RollupId

	tq            *TransactionQueue
	lastBatchHash []byte

	seenBatches map[string]struct{}
	bq          *BatchQueue

	client *SEQClient
}

// NewSequencer ...
func NewSequencer(daAddress, daAuthToken, daNamespace string, batchTime time.Duration, seqClient *SEQClient) (*Sequencer, error) {
	ctx := context.Background()
	dac, err := proxyda.NewClient(daAddress, daAuthToken)
	if err != nil {
		return nil, fmt.Errorf("error while establishing connection to DA layer: %w", err)
	}
	dalc := da.NewDAClient(dac, -1, 0, goda.Namespace(daNamespace))
	maxBlobSize, err := dalc.DA.MaxBlobSize(ctx)
	if err != nil {
		return nil, err
	}
	s := &Sequencer{
		dalc:          dalc,
		batchTime:     batchTime,
		ctx:           ctx,
		maxDABlobSize: maxBlobSize,
		tq:            NewTransactionQueue(),
		bq:            NewBatchQueue(),
		client:        seqClient,
		seenBatches:   make(map[string]struct{}),
	}
	go s.batchSubmissionLoop(s.ctx)
	return s, nil
}

func (c *Sequencer) batchSubmissionLoop(ctx context.Context) {
	batchTimer := time.NewTimer(0)
	defer batchTimer.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-batchTimer.C:
		}
		start := time.Now()
		err := c.publishBatch()
		if err != nil && ctx.Err() == nil {
			log.Errorf("error while publishing block", "error", err)
		}
		batchTimer.Reset(getRemainingSleep(start, c.batchTime, 0))
	}
}

func (c *Sequencer) publishBatch() error {
	batch := c.tq.GetNextBatch(c.maxDABlobSize)
	err := c.submitBatchToDA(batch)
	if err != nil {
		return err
	}
	c.bq.AddBatch(batch)
	return nil
}

// wondering if submitBatchToDA is needed since we have the nodekit relayer,
// which submits the same info as batch(tx, namespace, height)
func (c *Sequencer) submitBatchToDA(batch sequencing.Batch) error {
	batchesToSubmit := []*sequencing.Batch{&batch}
	submittedAllBlocks := false
	var backoff time.Duration
	numSubmittedBatches := 0
	attempt := 0

	maxBlobSize := c.maxDABlobSize
	initialMaxBlobSize := maxBlobSize
	initialGasPrice := c.dalc.GasPrice
	gasPrice := c.dalc.GasPrice

daSubmitRetryLoop:
	for !submittedAllBlocks && attempt < maxSubmitAttempts {
		select {
		case <-c.ctx.Done():
			break daSubmitRetryLoop
		case <-time.After(backoff):
		}

		res := c.dalc.SubmitBatch(c.ctx, batchesToSubmit, maxBlobSize, gasPrice)
		switch res.Code {
		case da.StatusSuccess:
			txCount := 0
			for _, batch := range batchesToSubmit {
				txCount += len(batch.Transactions)
			}
			log.Info("successfully submitted batches to DA layer", "gasPrice", gasPrice, "daHeight", res.DAHeight, "batchCount", res.SubmittedCount, "txCount", txCount)
			if res.SubmittedCount == uint64(len(batchesToSubmit)) {
				submittedAllBlocks = true
			}
			submittedBatches, notSubmittedBatches := batchesToSubmit[:res.SubmittedCount], batchesToSubmit[res.SubmittedCount:]
			numSubmittedBatches += len(submittedBatches)
			batchesToSubmit = notSubmittedBatches
			// reset submission options when successful
			// scale back gasPrice gradually
			backoff = 0
			maxBlobSize = initialMaxBlobSize
			if c.dalc.GasMultiplier > 0 && gasPrice != -1 {
				gasPrice = gasPrice / c.dalc.GasMultiplier
				if gasPrice < initialGasPrice {
					gasPrice = initialGasPrice
				}
			}
			log.Debug("resetting DA layer submission options", "backoff", backoff, "gasPrice", gasPrice, "maxBlobSize", maxBlobSize)
		case da.StatusNotIncludedInBlock, da.StatusAlreadyInMempool:
			log.Error("DA layer submission failed", "error", res.Message, "attempt", attempt)
			backoff = c.batchTime * time.Duration(defaultMempoolTTL)
			if c.dalc.GasMultiplier > 0 && gasPrice != -1 {
				gasPrice = gasPrice * c.dalc.GasMultiplier
			}
			log.Info("retrying DA layer submission with", "backoff", backoff, "gasPrice", gasPrice, "maxBlobSize", maxBlobSize)

		case da.StatusTooBig:
			maxBlobSize = maxBlobSize / 4
			fallthrough
		default:
			log.Error("DA layer submission failed", "error", res.Message, "attempt", attempt)
			backoff = c.exponentialBackoff(backoff)
		}

		attempt += 1
	}

	if !submittedAllBlocks {
		return fmt.Errorf(
			"failed to submit all blocks to DA layer, submitted %d blocks (%d left) after %d attempts",
			numSubmittedBatches,
			len(batchesToSubmit),
			attempt,
		)
	}
	return nil
}

func (c *Sequencer) exponentialBackoff(backoff time.Duration) time.Duration {
	backoff *= 2
	if backoff == 0 {
		backoff = initialBackoff
	}
	if backoff > c.batchTime {
		backoff = c.batchTime
	}
	return backoff
}

func getRemainingSleep(start time.Time, blockTime time.Duration, sleep time.Duration) time.Duration {
	elapsed := time.Since(start)
	remaining := blockTime - elapsed
	if remaining < 0 {
		return 0
	}
	return remaining + sleep
}

func hashSHA256(data []byte) []byte {
	fmt.Printf("Hashing data: %x\n", data)
	hash := sha256.Sum256(data)
	fmt.Printf("Computed hash in hash func: %x\n", hash)
	return hash[:]
}

// SubmitRollupTransaction implements sequencing.Sequencer.
func (c *Sequencer) SubmitRollupTransaction(ctx context.Context, rollupId []byte, tx []byte) error {
	// rollupNamespace = make([]byte, 8)
	// binary.LittleEndian.PutUint64(rollupNamespace, rollupChainID)
	if c.rollupId == nil {
		c.rollupId = rollupId
	} else {
		if !bytes.Equal(c.rollupId, rollupId) {
			return ErrorRollupIdMismatch
		}
	}
	// need to make sure rollupId is length 8
	if len(rollupId) != 8 {
		if len(rollupId) > 8 {
			rollupId = rollupId[:8]
		} else {
			copy(rollupNamespace, rollupId)
			rollupId = rollupNamespace
		}
	}
	// create [][]byte with length 0 and capacity 1 to store tx(function arg)
	data := make([][]byte, 0, 1)
	// append the tx so data includes tx
	data = append(data, tx)
	fmt.Printf("data %v\n", data)
	// submit data, which includes tx, to SEQ
	// test with rollupNamespace or see if rollupId works on its own
	rollupTx, err := c.client.seqClient.SubmitTx(ctx, chainID, 1337, rollupId, data)
	if err != nil {
		fmt.Errorf("Error submitting tx(s) to SEQ: %v\n", err)
	}
	fmt.Printf("txs to seq %v\n", rollupTx)

	duplicate := false
	c.tq.mu.Lock()
	for _, queuedTx := range c.tq.queue {
		if bytes.Equal(queuedTx, tx) {
			duplicate = true
			break
		}
	}
	c.tq.mu.Unlock()

	if !duplicate {
		c.tq.AddTransaction(tx)
	}
	return nil
}

// GetNextBatch implements sequencing.Sequencer.
func (c *Sequencer) GetNextBatch(ctx context.Context, lastBatch *sequencing.Batch) (*sequencing.Batch, error) {
	// check the lastBatchHash to match the hash of the supplied lastBatch
	if lastBatch == nil && c.lastBatchHash != nil {
		return nil, errors.New("lastBatch is not supposed to be nil")
	}
	// bytes of last batch
	lastBatchBytes, err := lastBatch.Marshal()
	if err != nil {
		return nil, err
	}
	fmt.Printf("lastBatchBytes %v\n ", lastBatchBytes)
	// last batch hash
	lastBatchHash := hashSHA256(lastBatchBytes)
	c.lastBatchHash = lastBatchHash
	fmt.Printf("Computed lastBatchHash: %x\n", lastBatchHash)
	fmt.Printf("Stored lastBatchHash: %x\n", c.lastBatchHash)
	if !bytes.Equal(c.lastBatchHash, lastBatchHash) {
		return nil, errors.New("supplied lastBatch does not match with sequencer last batch")
	}
	// next batch in batch queue
	batch := c.bq.Next()
	if batch == nil {
		return nil, nil
	}
	fmt.Printf("next batch txs: %x\n", batch.Transactions)
	fmt.Printf("next batch in queue: %x\n", batch)
	// bytes of next batch
	batchBytes, err := batch.Marshal()
	if err != nil {
		return nil, err
	}
	fmt.Printf("next batch bytes: %x\n", batchBytes)
	// TODO: Figure out logic below so that func returns batch with newTxs only from the test file
	// note: lastBatch will need to include the tx(s) from mpoolTxs in test file only
	// use height from last batch
	blockHeight = lastBatch.Height
	fmt.Printf("last batch height: %x\n", blockHeight)
	// 1: take in blocks from height or ID of lastBatch.Height up until user made request(end) or a range 
	start := time.Now().UnixMilli()
	end := start - 60 * 1000
	args := info.GetBlockHeadersByStartArgs{Start: int64(blockHeight + 1), End: end}
	// 2: fetch the blocks within the range of args
	blockHeader, err := c.client.seqClient.GetBlockHeadersByStart(ctx, args.Start, args.End)
	if err != nil {
		return nil, fmt.Errorf("error fetching block headers: %v", err)
	}
	if len(blockHeader.Blocks) == 0 {
		return nil, fmt.Errorf("no block headers found")
	}
	rollupNamespace = make([]byte, 8)
	binary.LittleEndian.PutUint64(rollupNamespace, rollupChainID)
	namespaceStr := hex.EncodeToString(rollupNamespace)

	// case 1: seen txs from last batch stored in map like in sequencer struct
	lastBatchTxMap := make(map[string]struct{})
	for _, tx := range lastBatch.Transactions {
		txStr := hex.EncodeToString(tx)
		lastBatchTxMap[txStr] = struct{}{}
	}
	fmt.Printf("last batch txs map: %v\n", lastBatchTxMap)

	// case 2: duplicate txs from next batch stored in map like in sequencer struct
	// this is to avoid the same txs duplicates in next batch Transactions
	batchTxsMap := make(map[string]struct{})

	for _, block := range blockHeader.Blocks {
		// curr block needs to be greater than last batch height
		// that way we get only the txs from newTxs in test file
		if block.Height >= lastBatch.Height {
			// 3: extract tx(s) using height of the current block and namespace of rollup
			nsResp, err := c.client.seqClient.GetBlockTransactionsByNamespace(ctx, uint64(block.Height), namespaceStr)
			if err != nil {
				return nil, fmt.Errorf("error fetching block txs by namepspace: %v", err)
			}
			// fmt.Printf("block txs by ns: %v\n", nsResp)
			// fmt.Printf("txs by nsResp: %v\n", nsResp.Txs)
			// 4: after tx(s) are extracted, we append tx(s) to the next batch
			for _, tx := range nsResp.Txs {
				txStr := hex.EncodeToString(tx.Transaction)
				// fmt.Printf("Checking transaction: %x\n", tx.Transaction)
				// checks if last batch tx is included or not
				if _, exists := lastBatchTxMap[txStr]; !exists {
					// checks for duplicates txs in next batch. that way next batch are unique new txs only
					if _, same := batchTxsMap[txStr]; !same {
						batch.Transactions = append(batch.Transactions, tx.Transaction)
						batchTxsMap[txStr] = struct{}{}
						fmt.Printf("Added tx: %x\n", txStr)
						fmt.Printf("batchTxsMap after added loop %v\n", batchTxsMap)
					}
				}
			}
		}
	}
	fmt.Printf("Final Last batch map: %v\n", lastBatchTxMap)
	fmt.Printf("Final batch transactions: %v\n", batch.Transactions)

	c.lastBatchHash = hashSHA256(batchBytes)
	fmt.Printf("c.lastBatchHash aka next batch: %x\n", c.lastBatchHash)
	c.seenBatches[string(c.lastBatchHash)] = struct{}{}
	fmt.Printf("c.seenBatches(add next batch to seen): %x\n", c.seenBatches)
	batch.Namespace = namespaceStr
	batch.Height = blockHeight
	fmt.Printf("batch: %x\n", batch)
	// 5: return next batch
	return batch, nil
}

// VerifyBatch implements sequencing.///////////////¬≥':????????Sequencer.
func (c *Sequencer) VerifyBatch(ctx context.Context, batch *sequencing.Batch) (bool, error) {
	//TODO: need to add DA verification
	batchBytes, err := batch.Marshal()
	if err != nil {
		return false, err
	}
	// hash of batch
	hash := hashSHA256(batchBytes)
	// Check if the batch hash exists in the seenBatches map
	if _, ok := c.seenBatches[string(hash)]; ok {
		return true, nil
	}

	// double check logic below
	// 1: take batch which has array of tx, namespace, and height and get transactions by namepsace & height.
	namespaceStr := hex.EncodeToString(rollupNamespace)
	nsResp, err := c.client.seqClient.GetBlockTransactionsByNamespace(ctx, batch.Height, namespaceStr)
	if err != nil {
		return false, fmt.Errorf("Error retrieving namespace tx(s) by height")
	}
	// 2: compare batch object(batchTx) with seq tx response(seqTx) so essentially comparing both tx arrays
	for _, batchTx := range batch.Transactions {
		found := false
		for _, seqTx := range nsResp.Txs {
			if string(seqTx.Transaction) == string(batchTx) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	// if successful, add hash to seen
	c.seenBatches[string(hash)] = struct{}{}
	return true, nil
}

// RelayToDA listens for SEQ blocks and submits/retrieves from DA layer
func (c *Sequencer) RelayToDA(ctx context.Context) error {
	//setup relayer
	RPC := "127.0.0.1:12510"
	relay_uri := "http://" + RPC
	file := conf.SeqJsonRPCConfig{
		URI:       uri,
		NetworkID: 1337,
		ChainID:   chainID,
	}

	cli, err := relay.NewJSONRPCClient(relay_uri, file)
	if err != nil {
		return err
	}
	stable, err := cli.GetStableSeqHeight(context.Background())
	if err != nil {
		return err
	}
	fmt.Println("Returning Stable Seq Height ", "height", stable)

	blockHeight = uint64(0)
	daBlock, err := cli.GetSeqBlock(context.Background(), blockHeight)
	if err != nil {
		return err
	}
	fmt.Println("Returning DA SEQ Block", "PoB", daBlock)

	name, _, err := cli.GetNamespacedSeqBlock(context.Background(), []byte("main seq chain id"), blockHeight)
	if err != nil {
		return err
	}
	fmt.Println("Returning DA SEQ Namespaced Block", "PoB", name)

	return nil
}
