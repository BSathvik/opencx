package main

import (
	"fmt"
	"log"
	"net"
	"net/rpc"
	"os"

	flags "github.com/jessevdk/go-flags"
	"github.com/mit-dci/opencx/cxrpc"
	"github.com/mit-dci/opencx/cxserver"
	"github.com/mit-dci/opencx/db/ocxsql"
	"github.com/mit-dci/opencx/logging"
	"github.com/mit-dci/opencx/match"
)

type opencxConfig struct {
	ConfigFile string

	// stuff for files and directories
	LogFilename   string `long:"logFilename" description:"Filename for output log file"`
	OpencxHomeDir string `long:"dir" description:"Location of the root directory relative to home directory"`

	// stuff for ports
	Rpcport uint16 `short:"p" long:"rpcport" description:"Set RPC port to connect to"`
	Rpchost string `long:"rpchost" description:"Set RPC host to listen to"`

	// logging and debug parameters
	LogLevel []bool `short:"v" description:"Set verbosity level to verbose (-v), very verbose (-vv) or very very verbose (-vvv)"`

	// logging for lit nodes (find something better than w)
	LitLogLevel []bool `short:"w" description:"Set verbosity level to verbose (-w), very verbose (-ww) or very very verbose (-www)"`

	// networks that we can connect to
	// Tn3host     string `long:"tn3" description:"Connect to bitcoin testnet3. Specify a socket address."`
	// Lt4host     string `long:"lt4" description:"Connect to litecoin testnet4. Specify a socket address."`
	// Tvtchost    string `long:"tvtc" description:"Connect to Vertcoin test node. Specify a socket address."`
	Reghost     string `long:"reg" description:"Connect to bitcoin regtest. Specify a socket address."`
	Litereghost string `long:"litereg" description:"Connect to litecoin regtest. Specify a socket address."`
	Rtvtchost   string `long:"rtvtc" description:"Connect to Vertcoin regtest node. Specify a socket address."`
	MaxPeers    uint16 `long:"numpeers" description:"Maximum number of peers that you'd like to support"`
	MinPeerPort uint16 `long:"minpeerport" description:"Port to start creating ports for peers at"`
	Lithost     string `long:"lithost" description:"Host for the lightning node on the exchange to run"`
	Litport     uint16 `long:"litport" description:"Port for the lightning node on the exchange to run"`
}

var (
	defaultConfigFilename    = "opencx.conf"
	defaultLogFilename       = "dblog.txt"
	defaultOpencxHomeDirName = os.Getenv("HOME") + "/.opencx/"
	defaultKeyFileName       = "privkey.hex"
	defaultLogLevel          = 0
	defaultLitLogLevel       = 0
	defaultHomeDir           = os.Getenv("HOME")
	defaultRpcport           = uint16(12345)
	defaultRpchost           = "localhost"
	defaultMaxPeers          = uint16(64)
	defaultMinPeerPort       = uint16(25565)
	defaultReghost           = "yup"
	defaultLitereghost       = "yup"
	defaultRtvtchost         = "yup"
	defaultLithost           = "localhost"
	defaultLitport           = uint16(12346)
)

var orderBufferSize = 1

// newConfigParser returns a new command line flags parser.
func newConfigParser(conf *opencxConfig, options flags.Options) *flags.Parser {
	parser := flags.NewParser(conf, options)
	return parser
}

func main() {
	var err error

	conf := opencxConfig{
		OpencxHomeDir: defaultOpencxHomeDirName,
		Rpcport:       defaultRpcport,
		Rpchost:       defaultRpchost,
		Reghost:       defaultReghost,
		Litereghost:   defaultLitereghost,
		Rtvtchost:     defaultRtvtchost,
		MaxPeers:      defaultMaxPeers,
		MinPeerPort:   defaultMinPeerPort,
		Lithost:       defaultLithost,
		Litport:       defaultLitport,
	}

	// Check and load config params
	key := opencxSetup(&conf)

	db := new(ocxsql.DB)

	// Get all the pairs
	assetPairs := match.GenerateAssetPairs()

	// Get all the assets
	assets := match.AssetList()

	// Setup DB Client
	err = db.SetupClient(assets, assetPairs)
	if err != nil {
		log.Fatalf("Error setting up sql client: \n%s", err)
	}

	// defer the db closing to when we stop
	defer db.DBHandler.Close()

	// Anyways, here's where we set the server
	ocxServer := cxserver.InitServer(db, conf.OpencxHomeDir, conf.Rpcport, assetPairs, assets)

	// Check that the private key exists and if it does, load it
	if err = ocxServer.SetupServerKeys(key); err != nil {
		logging.Fatalf("Error setting up server keys: \n%s", err)
	}

	// start the lit node for the exchange
	if err = ocxServer.SetupLitNode(key, "http://hubris.media.mit.edu:46580", "", ""); err != nil {
		logging.Fatalf("Error starting lit node: \n%s", err)
	}

	btcCoinTypeChan := make(chan int, 1)
	ltcCoinTypeChan := make(chan int, 1)
	vtcCoinTypeChan := make(chan int, 1)
	hookErrorChannel := make(chan error, 3)
	// Set up all chain hooks
	go ocxServer.SetupBTCChainhook(hookErrorChannel, btcCoinTypeChan, conf.Reghost)
	go ocxServer.SetupLTCChainhook(hookErrorChannel, ltcCoinTypeChan, conf.Litereghost)
	go ocxServer.SetupVTCChainhook(hookErrorChannel, vtcCoinTypeChan, conf.Rtvtchost)

	// Wait until all hooks are started to do the rest
	for i := 0; i < 3; i++ {
		firstError := <-hookErrorChannel
		if firstError != nil {
			logging.Fatalf("Error when starting hook: \n%s", firstError)
		}
		logging.Infof("Started hook #%d\n", i+1)
	}

	btcCoinType := <-btcCoinTypeChan
	ltcCoinType := <-ltcCoinTypeChan
	vtcCoinType := <-vtcCoinTypeChan

	// Waited until the wallets are started, time to link them!
	if err = ocxServer.LinkAllWallets(btcCoinType, ltcCoinType, vtcCoinType); err != nil {
		logging.Fatalf("Could not link wallets: \n%s", err)
	}

	// Listen on a bunch of ports according to the number of peers you want to support.
	for portNum := conf.MinPeerPort; portNum < conf.MinPeerPort+conf.MaxPeers; portNum++ {
		var _ string
		if _, err = ocxServer.ExchangeNode.TCPListener(int(portNum)); err != nil {
			return
		}

		// logging.Infof("Listening for connections with address %s on port %d", addr, portNum)
	}

	// init the maps for the server
	ocxServer.InitMatchingMaps()

	// Get all the asset pairs then start the matching loop
	for i, pair := range assetPairs {
		go ocxServer.MatchingLoop(pair, orderBufferSize)
		logging.Debugf("Pair %d: %s\n", i, pair)
	}

	// Register RPC Commands and set server
	rpc1 := new(cxrpc.OpencxRPC)
	rpc1.Server = ocxServer

	if err = rpc.Register(rpc1); err != nil {
		logging.Fatalf("Error registering RPC Interface:\n%s", err)
	}

	// Start RPC Server
	var listener net.Listener
	if listener, err = net.Listen("tcp", conf.Rpchost+":"+fmt.Sprintf("%d", conf.Rpcport)); err != nil {
		logging.Fatal("listen error:", err)
	}
	logging.Infof("Running RPC server on %s\n", listener.Addr().String())

	// Setup lit node rpc
	go ocxServer.SetupLitRPCConnect(conf.Lithost, conf.Litport)

	defer listener.Close()
	rpc.Accept(listener)

}
