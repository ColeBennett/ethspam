package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync/atomic"
	"time"

	"github.com/INFURA/go-ethlibs/node"
	flags "github.com/jessevdk/go-flags"
)

// Version of the binary, assigned during build.
var Version = "dev"

// Options contains the flag options
type Options struct {
	Methods      map[string]int64 `short:"m" long:"method" description:"A map from json rpc methods to their weight" default:"eth_getCode:100" default:"eth_getLogs:250" default:"eth_getTransactionByHash:250" default:"eth_blockNumber:350" default:"eth_getTransactionCount:400" default:"eth_getBlockByNumber:400" default:"eth_getBalance:550" default:"eth_getTransactionReceipt:600" default:"eth_call:2000"`
	Web3Endpoint string           `long:"rpc" description:"Ethereum JSONRPC provider, such as Infura or Cloudflare" default:"https://mainnet.infura.io/v3/af500e495f2d4e7cbcae36d0bfa66bcb"` // Versus API key on Infura
	RateLimit    float64          `short:"r" long:"ratelimit" description:"rate limit for generating jsonrpc calls"`

	Version bool `long:"version" description:"Print version and exit."`
}

func exit(code int, format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(code)
}

func main() {
	options := Options{}
	p, err := flags.NewParser(&options, flags.Default).ParseArgs(os.Args[1:])
	if err != nil {
		if p == nil {
			fmt.Println(err)
		}
		return
	}

	if options.Version {
		fmt.Println(Version)
		os.Exit(0)
	}

	gen := generator{}
	err = installDefaults(&gen, options.Methods)
	if err != nil {
		exit(1, "failed to install defaults: %s", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := node.NewClient(ctx, options.Web3Endpoint)
	if err != nil {
		exit(1, "failed to make a new client: %s", err)
	}
	mkState := stateProducer{
		client: client,
	}

	// stateChannel 😂
	stateChannel := make(chan State, 1)

	// We don't need a high quality randomness source, just for benchmark shuffling
	randSrc := rand.NewSource(time.Now().UnixNano())
	go func() {
		state := liveState{
			idGen:   &idGenerator{},
			randSrc: randSrc,
		}
		for {
			newState, err := mkState.Refresh(&state)
			if err != nil {
				// It can happen in some testnets that most of the blocks
				// are empty(no transaction included), don't refresh the
				// generator state without new inclusion.
				if err == errEmptyBlock {
					select {
					case <-time.After(5 * time.Second):
					case <-ctx.Done():
						return
					}
					continue
				}
				exit(2, "failed to refresh state")
			}
			select {
			case stateChannel <- newState:
			case <-ctx.Done():
				return
			}

			select {
			case <-time.After(15 * time.Second):
			case <-ctx.Done():
			}
		}
	}()

	// var rlimit *rate.Limiter
	// if options.RateLimit != 0 {
	// 	rlimit = rate.NewLimiter(rate.Limit(options.RateLimit), 10)
	// }

	const numWorkers = 250

	for i := 0; i < numWorkers; i++ {
		go func() {
			buf := &bytes.Buffer{}
			state := <-stateChannel

			for {
				// Update state when a new one is emitted
				select {
				case state = <-stateChannel:
				case <-ctx.Done():
					return
				default:
				}
				// if rlimit != nil {
				// 	rlimit.Wait(context.Background())
				// }

				if err := gen.Query(buf, state); err == io.EOF {
					// Done
					fmt.Println("query gen EOF")
					return
				} else if err != nil {
					exit(2, "failed to write generated query: %s", err)
				} else {
					query(options.Web3Endpoint, buf)
				}

				buf.Reset()
			}
		}()
	}

	var prevCounter int64

	for {
		currentCounter := atomic.LoadInt64(&counter)
		reqsPerSecond := currentCounter - prevCounter
		prevCounter = currentCounter

		log.Printf("req/s :: %d\n", reqsPerSecond)

		if counter%100 == 0 {
			log.Printf("sent %d requests\n", counter)
		}

		time.Sleep(time.Second)
	}
}

var counter int64

func query(endpoint string, queryBuf *bytes.Buffer) {
	// log.Println(queryBuf.String())

	resp, err := http.Post(endpoint, "application/json", queryBuf)
	if err != nil {
		log.Printf("error: %s, status code: %d\n", err.Error(), resp.StatusCode)
	}

	atomic.AddInt64(&counter, 1)
}

type Generator func(io.Writer, State) error

type RandomQuery struct {
	Method   string
	Weight   int64
	Generate Generator
}

type generator struct {
	queries     []RandomQuery // sorted by weight asc
	totalWeight int64
}

// Add inserts a random query generator with a weighted probability. Not
// goroutine-safe, should be run once during initialization.
func (g *generator) Add(query RandomQuery) {
	if g.queries == nil {
		g.queries = make([]RandomQuery, 1)
	} else {
		g.queries = append(g.queries, RandomQuery{})
	}
	// Maintain weight sort
	idx := sort.Search(len(g.queries), func(i int) bool { return g.queries[i].Weight < query.Weight })
	copy(g.queries[idx+1:], g.queries[idx:])
	g.queries[idx] = query
	g.totalWeight += query.Weight
}

// Query selects a generator based on proportonal weighted probability and
// writes the query from the generator.
func (g *generator) Query(w io.Writer, s State) error {
	if len(g.queries) == 0 {
		return errors.New("no query generators available")
	}

	weight := s.RandInt64() % g.totalWeight

	var current int64
	for _, q := range g.queries {
		// TODO: Test for off-by-one
		current += q.Weight
		if current >= weight {
			return q.Generate(w, s)
		}
	}

	panic("off by one bug in weighted query selection")
}
