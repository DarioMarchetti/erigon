package main

import "time"

type movaAccount struct {
	Type  string `json:"type"`
	Value struct {
		Name         string      `json:"name"`
		Address      string      `json:"address"`
		SubAddresses interface{} `json:"sub_addresses"`
		Coins        []struct {
			Denom  string `json:"denom"`
			Amount string `json:"amount"`
		} `json:"coins"`
		PublicKey     interface{} `json:"public_key"`
		AccountNumber int         `json:"account_number"`
		Sequence      int         `json:"sequence"`
		CodeHash      string      `json:"code_hash"`
		EthAddress    string      `json:"ethAddress"`
	}
}
type movaGenesis struct {
	GenesisTime     time.Time `json:"genesis_time"`
	ChainId         string    `json:"chain_id"`
	ConsensusParams struct {
		Event struct {
			MaxGas     string `json:"max_gas"`
			MaxBytes   string `json:"max_bytes"`
			TimeIotaMs string `json:"time_iota_ms"`
		} `json:"event"`
	} `json:"consensus_params"`
	AppState struct {
		Auth struct {
			Accounts []movaAccount `json:"accounts"`
		} `json:"auth"`
		Evm struct {
			Params struct {
				EvmDenom    string `json:"evm_denom"`
				ChainConfig struct {
					HomesteadBlock      string `json:"homestead_block"`
					DaoForkBlock        string `json:"dao_fork_block"`
					DaoForkSupport      bool   `json:"dao_fork_support"`
					Eip150Block         string `json:"eip150_block"`
					Eip150Hash          string `json:"eip150_hash"`
					Eip155Block         string `json:"eip155_block"`
					Eip158Block         string `json:"eip158_block"`
					ByzantiumBlock      string `json:"byzantium_block"`
					ConstantinopleBlock string `json:"constantinople_block"`
					PetersburgBlock     string `json:"petersburg_block"`
					IstanbulBlock       string `json:"istanbul_block"`
					MuirGlacierBlock    string `json:"muir_glacier_block"`
					YoloV2Block         string `json:"yoloV2_block"`
					EwasmBlock          string `json:"ewasm_block"`
				} `json:"chain_config"`
			} `json:"params"`
		} `json:"evm"`
		Genutil struct {
			Gentxs []struct {
				Type  string `json:"type"`
				Value struct {
					Msg []struct {
						Type  string `json:"type"`
						Value struct {
							Description struct {
								Moniker         string `json:"moniker"`
								Identity        string `json:"identity"`
								Website         string `json:"website"`
								SecurityContact string `json:"security_contact"`
								Details         string `json:"details"`
							} `json:"description"`
							Commission struct {
								Rate          string `json:"rate"`
								MaxRate       string `json:"max_rate"`
								MaxChangeRate string `json:"max_change_rate"`
							} `json:"commission"`
							MinSelfDelegation string `json:"min_self_delegation"`
							DelegatorAddress  string `json:"delegator_address"`
							ValidatorAddress  string `json:"validator_address"`
							Pubkey            string `json:"pubkey"`
							Value             struct {
								Denom  string `json:"denom"`
								Amount string `json:"amount"`
							} `json:"value"`
						} `json:"value"`
					} `json:"msg"`
					Fee struct {
						Amount []interface{} `json:"amount"`
						Gas    string        `json:"gas"`
					} `json:"fee"`
					Signatures []struct {
						PubKey struct {
							Type  string `json:"type"`
							Value string `json:"value"`
						} `json:"pub_key"`
						Signature string `json:"signature"`
					} `json:"signatures"`
					Memo string `json:"memo"`
				} `json:"value"`
			} `json:"gentxs"`
		} `json:"genutil"`
	} `json:"app_state"`
}
