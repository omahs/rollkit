# Full Node

## Abstract

A Full Node is a top-level service that encapsulates different components of Rollkit and initializes/manages them. The main components are listed in the table below:

 Component     | Description                                                                                                                         |
 ----------    | ------------------------------------------------------------------------------------------------------------------------------------|
 proxyApp      | ProxyApp is the interface to the application that consists of multiple connections. It is used to communicate with the application.|
 genesisDoc    | GenesisDoc is the genesis document that contains information about the initial state of the blockchain.|
 conf          | Conf is the configuration of the node. It contains all the necessary settings for the node to be initialized and function properly.|
 P2P           | P2P is the peer-to-peer client used for communication between nodes in the network.|
 Mempool       | Mempool is the transaction pool where all the transactions are stored before they are added to a block.|
 Store         | Store is used to store/retrieve blocks, commits, and state.|
 blockManager  | BlockManager is responsible for managing the operations related to blocks such as creating and validating blocks.|
 dalc          | DALC is the Data Availability Layer Client used to interact with the data availability layer.|
 hExService    | HExService is the Header Exchange Service used for exchanging block headers between nodes over P2P.|
 bExService    | BExService is the Block Exchange Service used for exchanging blocks between nodes over P2P. |

## Details

A Full Node is initialized with a configuration, private keys, a client creator, a genesis document, and a logger. It uses them to set up the components described above. The Full Node also has methods for starting and stopping the node, getting the genesis document, and setting and getting the logger.

## Message Structure/Communication Format

The Full Node communicates with other nodes in the network using the P2P client. It also communicates with the application using the ABCI proxy connections. The communication format is based on the P2P and ABCI protocols.

## Assumptions and Considerations

The Full Node assumes that the configuration, private keys, client creator, genesis document, and logger are correctly set up. It also assumes that the P2P client, data availability layer client, mempool, block manager, and other services can be started and stopped without errors. It also assumes that the `node.go` and `manager.go` files are correctly implemented.

## Implementation

The implementation of the Full Node can be found in the file `node/full.go`. It uses the `node.go` and `manager.go` files to manage the node and block operations.

## References

The Full Node uses the `github.com/cometbft/cometbft`, `github.com/rollkit/rollkit`, and other packages. It also interacts with the `node.go` and `manager.go` files.
