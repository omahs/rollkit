# Node

## Abstract

The Full Node is a key component in the Rollkit network. It connects all the components and orchestrates their work. It is responsible for managing the P2P client, data availability layer client, mempool, block manager, and other services. It is initialized with a configuration, private keys, a client creator, a genesis document, and a logger.

## Protocol/Component Description

The Full Node is initialized with a configuration, private keys, a client creator, a genesis document, and a logger. It sets up the P2P client, data availability layer client, mempool, block manager, and other services. It also sets up the event bus and starts the proxy app connections. The Full Node also has methods for starting and stopping the node, getting the genesis document, and setting and getting the logger. It interacts with the `node.go` and `manager.go` files to manage the node and block operations.

## Message Structure/Communication Format

The Full Node communicates with other nodes in the network using the P2P client. It also communicates with the application using the ABCI proxy connections. The communication format is based on the P2P and ABCI protocols.

## Assumptions and Considerations

The Full Node assumes that the configuration, private keys, client creator, genesis document, and logger are correctly set up. It also assumes that the P2P client, data availability layer client, mempool, block manager, and other services can be started and stopped without errors. It also assumes that the `node.go` and `manager.go` files are correctly implemented.

## Implementation

The implementation of the Full Node can be found in the file `node/full.go`. It uses the `node.go` and `manager.go` files to manage the node and block operations.

## References

The Full Node uses the `github.com/cometbft/cometbft`, `github.com/rollkit/rollkit`, and other packages. It also interacts with the `node.go` and `manager.go` files.

