package sequencing

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	proxyda "github.com/rollkit/go-da/proxy"
	"github.com/rollkit/go-sequencing"

	trpc "github.com/AnomalyFi/seq-sdk/client"
	info "github.com/AnomalyFi/seq-sdk/types"
	conf "github.com/AnomalyFi/nodekit-relay/config"
	relay "github.com/AnomalyFi/nodekit-relay/rpc"
)

// NodeKit Vars
var chainID = "hCTcJQm6811V9Suj6XomjXEcszEPLpG3nD4dRWUWUQHgZRWbJ"
var uri = "http://54.175.18.95:9650/ext/bc/hCTcJQm6811V9Suj6XomjXEcszEPLpG3nD4dRWUWUQHgZRWbJ"
// each rollup will have a specific chain ID
var rollupChainID = uint64(45200)
var rollupNamespace = make([]byte, 8)
// below is how the chain id and namespace of rollup are stored into a unique byte array
// important for fetching txs by namespace and height
var cli = NewSEQClient(uri, chainID)
var blockHeight = uint64(0)

// NodeKit Client
type SEQClient struct {
	seqClient *trpc.JSONRPCClient
}

func NewSEQClient(url string, id string) *SEQClient {
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}

	cli := trpc.NewJSONRPCClient(url, 1337, id)

	return &SEQClient{
		seqClient: cli,
	}
}

var _ sequencing.Sequencer = &Sequencer{}

// Sequencer implements go-sequencing interface using celestia backend
type Sequencer struct {
	client *SEQClient
}

func NewSequencer(daAddress, daAuthToken, daNamespace string, seqClient *SEQClient) (*Sequencer, error) {
	_, err := proxyda.NewClient(daAddress, daAuthToken)
	if err != nil {
		return nil, fmt.Errorf("error while establishing connection to DA layer: %w", err)
	}
	return &Sequencer{client: seqClient}, nil
}

// SubmitRollupTransaction implements sequencing.Sequencer: submits rollup txs to SEQ
func (c *Sequencer) SubmitRollupTransaction(ctx context.Context, rollupId []byte, tx []byte) error {
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
	// submit data, which includes tx, to SEQ 
	// test with rollupNamespace or see if rollupId works on its own
	_, err := c.client.seqClient.SubmitTx(ctx, chainID, 1337, rollupNamespace, data)
	if err != nil {
		fmt.Errorf("Error submitting tx(s) to SEQ: %v\n", err)
	}
	return nil
}

// GetNextBatch implements sequencing.Sequencer: returns the next batch of transactions from sequencer to rollup.
func (c *Sequencer) GetNextBatch(ctx context.Context, lastBatch sequencing.Batch) (sequencing.Batch, error) {
	// note: lastBatch will need to include the tx(s) from SEQ
	blockHeight = lastBatch.Height
	binary.LittleEndian.PutUint64(rollupNamespace, rollupChainID)
	// 1: take height of lastBatch up until user made request(end)
	start := time.Now().UnixMilli()
	end := start - 120 * 1000

	args := info.GetBlockHeadersByHeightArgs{Height: blockHeight, End: end}

	// Fetch the block within range lastBatch.Height and end
	blockHeader, err := c.client.seqClient.GetBlockHeadersByHeight(ctx, args.Height, args.End)
	if err != nil {
		fmt.Errorf("error fetching block headers: %v", err)
	}
	if len(blockHeader.Blocks) == 0 {
		fmt.Errorf("no block headers found")
	}

	// next batch of transactions in block at lastBatch.Height
	var nextBatch sequencing.Batch
	nsResp, err := c.client.seqClient.GetBlockTransactionsByNamespace(ctx, blockHeight, string(rollupNamespace))
	if err != nil {
		fmt.Errorf("Error retrieving namespace tx(s) by height")
	}
	for _, tx := range nsResp.Txs {
		// todo: needs logic checking
		// rollkit gave us lastBatch which is []Tx array([][]byte) so i'm assuming they want to exclude the first tx
		// and to include all following tx(s) into next batch.
		if tx.Index > 0 {
			nextBatch.Transactions = append(nextBatch.Transactions, tx.Transaction)
		}
	}
	if len(nextBatch.Transactions) == 0 {
		fmt.Errorf("next batch is empty")
	}
	
	return nextBatch, nil
}

// VerifyBatch implements sequencing.Sequencer: verifies a batch of transactions received from the sequencer.
func (c *Sequencer) VerifyBatch(ctx context.Context, batch sequencing.Batch) (bool, error) {
	// retrieves batch needing verifying from block at batch.Height
	nsResp, err := c.client.seqClient.GetBlockTransactionsByNamespace(ctx, batch.Height, string(batch.Namespace))
	if err != nil {
		fmt.Errorf("Error retrieving namespace tx(s) by height")
	}
	// verify batch by comparing data of seq tx(s)
	for _, batchTx := range batch.Transactions {
		confirmed := false
		for _, seqTx := range nsResp.Txs {
			if seqTx.Namespace == batch.Namespace && bytes.Equal(seqTx.Transaction, batchTx) {
				confirmed = true
				break
			}
		}
		if !confirmed {
			return false, nil
		}
	}
	return true, nil
}

// RelayToDA listens for SEQ blocks and submits/retrieves from DA layer
func (c *Sequencer) RelayToDA(ctx context.Context) error {
	//setup relayer
	RPC := "127.0.0.1:12510"
	relay_uri := "http://"+RPC
	file := conf.SeqJsonRPCConfig {
		URI: uri,
		NetworkID: 1337,
		ChainID: chainID,
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

	name, _, err := cli.GetNamespacedSeqBlock(context.Background(), []byte("hCTcJQm6811V9Suj6XomjXEcszEPLpG3nD4dRWUWUQHgZRWbJ"), blockHeight)
	if err != nil {
		return err
	}
	fmt.Println("Returning DA SEQ Namespaced Block", "PoB", name)

	return nil
}