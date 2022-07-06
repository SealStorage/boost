module github.com/filecoin-project/boost

go 1.16

replace github.com/filecoin-project/filecoin-ffi => ./extern/filecoin-ffi

// replace github.com/filecoin-project/lotus => ../lotus

require (
	contrib.go.opencensus.io/exporter/prometheus v0.4.0
	github.com/BurntSushi/toml v1.1.0
	github.com/alecthomas/jsonschema v0.0.0-20200530073317-71f438968921
	github.com/buger/goterm v1.0.3
	github.com/chzyer/readline v0.0.0-20180603132655-2972be24d48e
	github.com/davecgh/go-spew v1.1.1
	github.com/docker/go-units v0.4.0
	github.com/dustin/go-humanize v1.0.0
	github.com/etclabscore/go-openrpc-reflect v0.0.36
	github.com/fatih/color v1.13.0
	github.com/filecoin-project/dagstore v0.5.2
	github.com/filecoin-project/go-address v1.0.0
	github.com/filecoin-project/go-bitfield v0.2.4
	github.com/filecoin-project/go-cbor-util v0.0.1
	github.com/filecoin-project/go-commp-utils v0.1.3
	github.com/filecoin-project/go-data-transfer v1.15.2
	github.com/filecoin-project/go-fil-commcid v0.1.0
	github.com/filecoin-project/go-fil-commp-hashhash v0.1.0
	github.com/filecoin-project/go-fil-markets v1.23.0
	github.com/filecoin-project/go-jsonrpc v0.1.5
	github.com/filecoin-project/go-padreader v0.0.1
	github.com/filecoin-project/go-paramfetch v0.0.4
	github.com/filecoin-project/go-state-types v0.1.10
	github.com/filecoin-project/go-statestore v0.2.0
	github.com/filecoin-project/index-provider v0.8.1
	github.com/filecoin-project/lotus v1.17.0-rc1.0.20220706101903-9bcefce07b7c
	github.com/filecoin-project/specs-actors v0.9.15
	github.com/filecoin-project/specs-actors/v8 v8.0.1
	github.com/filecoin-project/specs-storage v0.4.1
	github.com/filecoin-project/storetheindex v0.4.17
	github.com/gbrlsnchs/jwt/v3 v3.0.1
	github.com/golang/mock v1.6.0
	github.com/google/uuid v1.3.0
	github.com/gorilla/mux v1.7.4
	github.com/graph-gophers/graphql-go v1.2.0
	github.com/graph-gophers/graphql-transport-ws v0.0.2
	github.com/hashicorp/go-multierror v1.1.1
	github.com/ipfs/go-block-format v0.0.3
	github.com/ipfs/go-blockservice v0.3.0
	github.com/ipfs/go-cid v0.2.0
	github.com/ipfs/go-cidutil v0.1.0
	github.com/ipfs/go-datastore v0.5.1
	github.com/ipfs/go-graphsync v0.13.1
	github.com/ipfs/go-ipfs-blockstore v1.2.0
	github.com/ipfs/go-ipfs-blocksutil v0.0.1
	github.com/ipfs/go-ipfs-chunker v0.0.5
	github.com/ipfs/go-ipfs-ds-help v1.1.0
	github.com/ipfs/go-ipfs-exchange-interface v0.1.0
	github.com/ipfs/go-ipfs-exchange-offline v0.2.0
	github.com/ipfs/go-ipfs-files v0.1.1
	github.com/ipfs/go-ipld-format v0.4.0
	github.com/ipfs/go-log/v2 v2.5.1
	github.com/ipfs/go-merkledag v0.6.0
	github.com/ipfs/go-metrics-interface v0.0.1
	github.com/ipfs/go-unixfs v0.3.1
	github.com/ipld/go-car v0.4.1-0.20220706093353-3d969443294c
	github.com/ipld/go-car/v2 v2.4.1-0.20220706093353-3d969443294c
	github.com/ipld/go-ipld-prime v0.17.0
	github.com/ipld/go-ipld-selector-text-lite v0.0.1
	github.com/jbenet/go-random v0.0.0-20190219211222-123a90aedc0c
	github.com/jpillora/backoff v1.0.0
	github.com/kelseyhightower/envconfig v1.4.0
	github.com/libp2p/go-eventbus v0.2.1
	github.com/libp2p/go-libp2p v0.20.1
	github.com/libp2p/go-libp2p-core v0.16.1
	github.com/libp2p/go-libp2p-gostream v0.3.2-0.20220309102559-3d4abe2a19ac
	github.com/libp2p/go-libp2p-http v0.2.1
	github.com/libp2p/go-libp2p-kad-dht v0.15.0
	github.com/libp2p/go-libp2p-peerstore v0.7.0
	github.com/libp2p/go-libp2p-pubsub v0.7.1
	github.com/libp2p/go-libp2p-record v0.1.3
	github.com/mattn/go-sqlite3 v1.14.10
	github.com/mitchellh/go-homedir v1.1.0
	github.com/multiformats/go-multiaddr v0.5.0
	github.com/multiformats/go-multibase v0.0.3
	github.com/multiformats/go-multihash v0.1.0
	github.com/multiformats/go-varint v0.0.6
	github.com/open-rpc/meta-schema v0.0.0-20201029221707-1b72ef2ea333
	github.com/pressly/goose/v3 v3.5.3
	github.com/prometheus/client_golang v1.12.1
	github.com/raulk/clock v1.1.0
	github.com/raulk/go-watchdog v1.2.0
	github.com/stretchr/testify v1.7.1
	github.com/urfave/cli/v2 v2.8.1
	github.com/whyrusleeping/base32 v0.0.0-20170828182744-c30ac30633cc
	github.com/whyrusleeping/cbor-gen v0.0.0-20220323183124-98fa8256a799
	go.opencensus.io v0.23.0
	go.uber.org/atomic v1.9.0
	go.uber.org/fx v1.15.0
	go.uber.org/multierr v1.8.0
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	golang.org/x/tools v0.1.11
	golang.org/x/xerrors v0.0.0-20220411194840-2f41105eb62f
	gopkg.in/cheggaaa/pb.v1 v1.0.28
)
