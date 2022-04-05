#!/bin/sh

set -o errexit -o nounset

CHAINID="test"
app="./celestia-appd"

# Build genesis file incl account for passed address
coins="1000000000000000uceles"
$app init $CHAINID --chain-id $CHAINID 
$app keys add validator --keyring-backend="test"
# this won't work because the some proto types are decalared twice and the logs output to stdout (dependency hell involving iavl)
$app add-genesis-account $($app keys show validator -a --keyring-backend="test") $coins
$app gentx validator 5000000000uceles --keyring-backend="test" --chain-id $CHAINID
$app collect-gentxs

# Set proper defaults and change ports
sed 's#"tcp://127.0.0.1:26657"#"tcp://0.0.0.0:26657"#g' ~/.celestia-app/config/config.toml
sed 's/timeout_commit = "5s"/timeout_commit = "1s"/g' ~/.celestia-app/config/config.toml
sed 's/timeout_propose = "3s"/timeout_propose = "1s"/g' ~/.celestia-app/config/config.toml
sed 's/index_all_keys = false/index_all_keys = true/g' ~/.celestia-app/config/config.toml

# Start the celestia-app
$app start --grpc.enable
