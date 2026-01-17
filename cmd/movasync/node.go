package main

import (
	"context"
	enode "github.com/erigontech/erigon/cmd/erigon/node"
	"github.com/erigontech/erigon/cmd/utils"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/version"
	"github.com/erigontech/erigon/node"
	"github.com/erigontech/erigon/node/nodecfg"
	"github.com/urfave/cli/v2"
)

func NewNodeConfig(ctx *cli.Context, logger log.Logger) (*nodecfg.Config, error) {
	nodeConfig, err := enode.NewNodConfigUrfave(ctx, nil, logger)
	if err != nil {
		return nil, err
	}

	// see similar changes in `cmd/geth/config.go#defaultNodeConfig`
	if commit := version.GitCommit; commit != "" {
		nodeConfig.Version = version.VersionWithCommit(commit)
	} else {
		nodeConfig.Version = version.VersionNoMeta
	}

	nodeConfig.IPCPath = "" // force-disable IPC endpoint
	nodeConfig.Name = "erigon"

	if ctx.IsSet(utils.DataDirFlag.Name) {
		nodeConfig.Dirs = datadir.New(ctx.String(utils.DataDirFlag.Name))
	}

	return nodeConfig, nil
}

func MakeNodeWithDefaultConfig(cliCtx *cli.Context, logger log.Logger) (*node.Node, error) {
	conf, err := NewNodeConfig(cliCtx, logger)
	if err != nil {
		return nil, err
	}
	return makeConfigNode(cliCtx.Context, conf, logger), nil
}

func makeConfigNode(ctx context.Context, config *nodecfg.Config, logger log.Logger) *node.Node {
	stack, err := node.New(ctx, config, logger)
	if err != nil {
		utils.Fatalf("Failed to create Erigon node: %v", err)
	}

	return stack
}
