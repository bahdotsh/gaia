package e2e

import (
	"bytes"
	"context"
	b64 "encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/cosmos/cosmos-sdk/server"
	srvconfig "github.com/cosmos/cosmos-sdk/server/config"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	authvesting "github.com/cosmos/cosmos-sdk/x/auth/vesting/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	genutiltypes "github.com/cosmos/cosmos-sdk/x/genutil/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	"github.com/cosmos/gaia/v8/app/params"
	ibcclienttypes "github.com/cosmos/ibc-go/v5/modules/core/02-client/types"
	ibcchanneltypes "github.com/cosmos/ibc-go/v5/modules/core/04-channel/types"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/suite"
	tmconfig "github.com/tendermint/tendermint/config"
	tmjson "github.com/tendermint/tendermint/libs/json"
	"github.com/tendermint/tendermint/libs/rand"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

const (
	photonDenom    = "photon"
	uatomDenom     = "uatom"
	initBalanceStr = "110000000000stake,100000000000000000photon,100000000000000000uatom"
	minGasPrice    = "0.00001"
	// the test globalfee in genesis is the same as minGasPrice
	// global fee lower/higher than min_gas_price
	initialGlobalFeeAmt    = "0.00001"
	lowGlobalFeesAmt       = "0.000001"
	highGlobalFeeAmt       = "0.0001"
	gas                    = 200000
	govProposalBlockBuffer = 35
	relayerAccountIndex    = 0
	icaOwnerAccountIndex   = 1
)

var (
	govSendMsgRecipientAddress = Address()
	stakingAmount              = math.NewInt(100000000000)
	stakingAmountCoin          = sdk.NewCoin(uatomDenom, stakingAmount)
	tokenAmount                = sdk.NewCoin(uatomDenom, math.NewInt(3300000000)) // 3,300uatom
	fees                       = sdk.NewCoin(uatomDenom, math.NewInt(330000))     // 0.33uatom
	depositAmount              = sdk.NewCoin(uatomDenom, math.NewInt(10000000))   // 10uatom
	distModuleAddress          = authtypes.NewModuleAddress(distrtypes.ModuleName).String()
	govModuleAddress           = authtypes.NewModuleAddress(govtypes.ModuleName).String()
	proposalCounter            = 0
	sendGovAmount              = sdk.NewInt64Coin(uatomDenom, 10)
)

type UpgradePlan struct {
	Name   string `json:"name"`
	Height int    `json:"height"`
	Info   string `json:"info"`
}

type SoftwareUpgrade struct {
	Type      string      `json:"@type"`
	Authority string      `json:"authority"`
	Plan      UpgradePlan `json:"plan"`
}

type CancelSoftwareUpgrade struct {
	Type      string `json:"@type"`
	Authority string `json:"authority"`
}

type IntegrationTestSuite struct {
	suite.Suite

	tmpDirs        []string
	chainA         *chain
	chainB         *chain
	dkrPool        *dockertest.Pool
	dkrNet         *dockertest.Network
	hermesResource *dockertest.Resource
	valResources   map[string][]*dockertest.Resource
}

type AddressResponse struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Address  string `json:"address"`
	Mnemonic string `json:"mnemonic"`
}

type GroupMember struct {
	Address  string `json:"address"`
	Weight   string `json:"weight"`
	Metadata string `json:"metadata"`
}

type MsgSend struct {
	Type   string     `json:"@type"`
	From   string     `json:"from_address"`
	To     string     `json:"to_address"`
	Amount []sdk.Coin `json:"amount"`
}

type ThresholdPolicy struct {
	Type      string               `json:"@type"`
	Threshold string               `json:"threshold"`
	Windows   DecisionPolicyWindow `json:"windows"`
}

type PercentagePolicy struct {
	Type       string               `json:"@type"`
	Percentage string               `json:"percentage"`
	Windows    DecisionPolicyWindow `json:"windows"`
}

type DecisionPolicyWindow struct {
	VotingPeriod       string `json:"voting_period"`
	MinExecutionPeriod string `json:"min_execution_period"`
}

func TestIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(IntegrationTestSuite))
}

func (s *IntegrationTestSuite) SetupSuite() {
	s.T().Log("setting up e2e integration test suite...")

	var err error
	s.chainA, err = newChain()
	s.Require().NoError(err)

	s.chainB, err = newChain()
	s.Require().NoError(err)

	s.dkrPool, err = dockertest.NewPool("")
	s.Require().NoError(err)

	s.dkrNet, err = s.dkrPool.CreateNetwork(fmt.Sprintf("%s-%s-testnet", s.chainA.id, s.chainB.id))
	s.Require().NoError(err)

	s.valResources = make(map[string][]*dockertest.Resource)

	vestingMnemonic, err := createMnemonic()
	s.Require().NoError(err)

	// The boostrapping phase is as follows:
	//
	// 1. Initialize Gaia validator nodes.
	// 2. Create and initialize Gaia validator genesis files (both chains)
	// 3. Start both networks.
	// 4. Create and run IBC relayer (Hermes) containers.

	s.T().Logf("starting e2e infrastructure for chain A; chain-id: %s; datadir: %s", s.chainA.id, s.chainA.dataDir)
	s.initNodes(s.chainA)
	s.initGenesis(s.chainA, vestingMnemonic)
	s.initValidatorConfigs(s.chainA)
	s.runValidators(s.chainA, 0)

	s.T().Logf("starting e2e infrastructure for chain B; chain-id: %s; datadir: %s", s.chainB.id, s.chainB.dataDir)
	s.initNodes(s.chainB)
	s.initGenesis(s.chainB, vestingMnemonic)
	s.initValidatorConfigs(s.chainB)
	s.runValidators(s.chainB, 10)

	time.Sleep(10 * time.Second)
	s.runIBCRelayer()
}

func (s *IntegrationTestSuite) TearDownSuite() {
	if str := os.Getenv("GAIA_E2E_SKIP_CLEANUP"); len(str) > 0 {
		skipCleanup, err := strconv.ParseBool(str)
		s.Require().NoError(err)

		if skipCleanup {
			return
		}
	}

	s.T().Log("tearing down e2e integration test suite...")

	s.Require().NoError(s.dkrPool.Purge(s.hermesResource))

	for _, vr := range s.valResources {
		for _, r := range vr {
			s.Require().NoError(s.dkrPool.Purge(r))
		}
	}

	s.Require().NoError(s.dkrPool.RemoveNetwork(s.dkrNet))

	os.RemoveAll(s.chainA.dataDir)
	os.RemoveAll(s.chainB.dataDir)

	for _, td := range s.tmpDirs {
		os.RemoveAll(td)
	}
}

func (s *IntegrationTestSuite) initNodes(c *chain) {
	s.Require().NoError(c.createAndInitValidators(2))
	/* Adding 4 accounts to val0 local directory
	c.genesisAccounts[0]: Relayer Wallet
	c.genesisAccounts[1]: ICA Owner
	c.genesisAccounts[2]: Test Account 1
	c.genesisAccounts[3]: Test Account 2
	*/
	s.Require().NoError(c.addAccountFromMnemonic(4))
	// Initialize a genesis file for the first validator
	val0ConfigDir := c.validators[0].configDir()
	addrAll := []sdk.AccAddress{}
	for _, val := range c.validators {
		address, err := val.keyInfo.GetAddress()
		s.Require().NoError(err)
		addrAll = append(addrAll, address)
	}

	for _, addr := range c.genesisAccounts {
		acctAddr, err := addr.keyInfo.GetAddress()
		s.Require().NoError(err)
		addrAll = append(addrAll, acctAddr)
	}

	s.Require().NoError(
		modifyGenesis(val0ConfigDir, "", initBalanceStr, addrAll, initialGlobalFeeAmt+uatomDenom, uatomDenom),
	)
	// copy the genesis file to the remaining validators
	for _, val := range c.validators[1:] {
		_, err := copyFile(
			filepath.Join(val0ConfigDir, "config", "genesis.json"),
			filepath.Join(val.configDir(), "config", "genesis.json"),
		)
		s.Require().NoError(err)
	}
}

// TODO find a better way to manipulate accounts to add genesis accounts
func (s *IntegrationTestSuite) generateAuthAndBankState(
	c *chain,
	vestingMnemonic string,
	appGenState map[string]json.RawMessage,
) ([]byte, []byte) {
	var (
		authGenState  = authtypes.GetGenesisStateFromAppState(cdc, appGenState)
		bankGenState  = banktypes.GetGenesisStateFromAppState(cdc, appGenState)
		val0ConfigDir = c.validators[0].configDir()
	)
	kb, err := keyring.New(keyringAppName, keyring.BackendTest, val0ConfigDir, nil, cdc)
	s.Require().NoError(err)

	keyringAlgos, _ := kb.SupportedAlgorithms()
	algo, err := keyring.NewSigningAlgoFromString(string(hd.Secp256k1Type), keyringAlgos)
	s.Require().NoError(err)

	// Use the first wallet from the same mnemonic by HD path
	account, err := kb.NewAccount("continuous_vesting", vestingMnemonic, "", HDPathZero, algo)
	s.Require().NoError(err)
	c.continuousVestingAcc, err = account.GetAddress()
	s.Require().NoError(err)
	s.T().Logf("created vesting continuous genesis account %s\n", c.continuousVestingAcc.String())

	// add continuous vesting account to the genesis
	baseVestingContinuousAccount := authtypes.NewBaseAccount(
		c.continuousVestingAcc, nil, 0, 0)
	vestingContinuousGenAccount := authvesting.NewContinuousVestingAccountRaw(
		authvesting.NewBaseVestingAccount(
			baseVestingContinuousAccount,
			sdk.NewCoins(vestingAmountVested),
			time.Now().Add(time.Duration(rand.Intn(100)+150)*time.Second).Unix(),
		),
		time.Now().Add(time.Duration(rand.Intn(60)+90)*time.Second).Unix(),
	)
	s.Require().NoError(vestingContinuousGenAccount.Validate())

	// Use the second wallet from the same mnemonic by HD path
	account, err = kb.NewAccount("delayed_vesting", vestingMnemonic, "", HDPathOne, algo)
	s.Require().NoError(err)
	c.delayedVestingAcc, err = account.GetAddress()
	s.Require().NoError(err)
	s.T().Logf("created vesting delayed genesis account %s\n", c.delayedVestingAcc.String())

	// add delayed vesting account to the genesis
	baseVestingDelayedAccount := authtypes.NewBaseAccount(
		c.delayedVestingAcc, nil, 0, 0)
	vestingDelayedGenAccount := authvesting.NewDelayedVestingAccountRaw(
		authvesting.NewBaseVestingAccount(
			baseVestingDelayedAccount,
			sdk.NewCoins(vestingAmountVested),
			time.Now().Add(time.Duration(rand.Intn(60)+90)*time.Second).Unix(),
		),
	)
	s.Require().NoError(vestingDelayedGenAccount.Validate())

	// unpack and append accounts
	accs, err := authtypes.UnpackAccounts(authGenState.Accounts)
	s.Require().NoError(err)
	accs = append(accs, vestingContinuousGenAccount, vestingDelayedGenAccount)
	accs = authtypes.SanitizeGenesisAccounts(accs)
	genAccs, err := authtypes.PackAccounts(accs)
	s.Require().NoError(err)
	authGenState.Accounts = genAccs

	// update auth module state
	auth, err := cdc.MarshalJSON(&authGenState)
	s.Require().NoError(err)

	// update balances
	vestingContinuousBalances := banktypes.Balance{
		Address: c.continuousVestingAcc.String(),
		Coins:   vestingBalance,
	}
	vestingDelayedBalances := banktypes.Balance{
		Address: c.delayedVestingAcc.String(),
		Coins:   vestingBalance,
	}
	bankGenState.Balances = append(bankGenState.Balances, vestingContinuousBalances, vestingDelayedBalances)
	bankGenState.Balances = banktypes.SanitizeGenesisBalances(bankGenState.Balances)

	// update the denom metadata for the bank module
	bankGenState.DenomMetadata = append(bankGenState.DenomMetadata, banktypes.Metadata{
		Description: "An example stable token",
		Display:     uatomDenom,
		Base:        uatomDenom,
		Symbol:      uatomDenom,
		Name:        uatomDenom,
		DenomUnits: []*banktypes.DenomUnit{
			{
				Denom:    uatomDenom,
				Exponent: 0,
			},
		},
	})

	// update bank module state
	bank, err := cdc.MarshalJSON(bankGenState)
	s.Require().NoError(err)

	return bank, auth
}

func (s *IntegrationTestSuite) initGenesis(c *chain, vestingMnemonic string) {
	serverCtx := server.NewDefaultContext()
	config := serverCtx.Config

	config.SetRoot(c.validators[0].configDir())
	config.Moniker = c.validators[0].moniker

	genFilePath := config.GenesisFile()
	appGenState, genDoc, err := genutiltypes.GenesisStateFromGenFile(genFilePath)
	s.Require().NoError(err)

	bankGenState, authGenState := s.generateAuthAndBankState(c, vestingMnemonic, appGenState)
	appGenState[authtypes.ModuleName] = authGenState
	appGenState[banktypes.ModuleName] = bankGenState

	var genUtilGenState genutiltypes.GenesisState
	s.Require().NoError(cdc.UnmarshalJSON(appGenState[genutiltypes.ModuleName], &genUtilGenState))

	// generate genesis txs
	genTxs := make([]json.RawMessage, len(c.validators))
	for i, val := range c.validators {
		createValmsg, err := val.buildCreateValidatorMsg(stakingAmountCoin)
		s.Require().NoError(err)
		signedTx, err := val.signMsg(createValmsg)

		s.Require().NoError(err)

		txRaw, err := cdc.MarshalJSON(signedTx)
		s.Require().NoError(err)

		genTxs[i] = txRaw
	}

	genUtilGenState.GenTxs = genTxs

	bz, err := cdc.MarshalJSON(&genUtilGenState)
	s.Require().NoError(err)
	appGenState[genutiltypes.ModuleName] = bz

	bz, err = json.MarshalIndent(appGenState, "", "  ")
	s.Require().NoError(err)

	genDoc.AppState = bz

	bz, err = tmjson.MarshalIndent(genDoc, "", "  ")
	s.Require().NoError(err)

	vestingPeriod, err := generateVestingPeriod()
	s.Require().NoError(err)

	// write the updated genesis file to each validator.
	for _, val := range c.validators {
		err = writeFile(filepath.Join(val.configDir(), "config", "genesis.json"), bz)
		s.Require().NoError(err)

		err = writeFile(filepath.Join(val.configDir(), vestingPeriodFilePath), vestingPeriod)
		s.Require().NoError(err)
	}
}

// initValidatorConfigs initializes the validator configs for the given chain.
func (s *IntegrationTestSuite) initValidatorConfigs(c *chain) {
	for i, val := range c.validators {
		tmCfgPath := filepath.Join(val.configDir(), "config", "config.toml")

		vpr := viper.New()
		vpr.SetConfigFile(tmCfgPath)
		s.Require().NoError(vpr.ReadInConfig())

		valConfig := tmconfig.DefaultConfig()

		s.Require().NoError(vpr.Unmarshal(valConfig))

		valConfig.P2P.ListenAddress = "tcp://0.0.0.0:26656"
		valConfig.P2P.AddrBookStrict = false
		valConfig.P2P.ExternalAddress = fmt.Sprintf("%s:%d", val.instanceName(), 26656)
		valConfig.RPC.ListenAddress = "tcp://0.0.0.0:26657"
		valConfig.StateSync.Enable = false
		valConfig.LogLevel = "info"

		var peers []string

		for j := 0; j < len(c.validators); j++ {
			if i == j {
				continue
			}

			peer := c.validators[j]
			peerID := fmt.Sprintf("%s@%s%d:26656", peer.nodeKey.ID(), peer.moniker, j)
			peers = append(peers, peerID)
		}

		valConfig.P2P.PersistentPeers = strings.Join(peers, ",")

		tmconfig.WriteConfigFile(tmCfgPath, valConfig)

		// set application configuration
		appCfgPath := filepath.Join(val.configDir(), "config", "app.toml")

		appConfig := srvconfig.DefaultConfig()
		appConfig.API.Enable = true
		appConfig.MinGasPrices = fmt.Sprintf("%s%s", minGasPrice, uatomDenom)

		//	 srvconfig.WriteConfigFile(appCfgPath, appConfig)
		appCustomConfig := params.CustomAppConfig{
			Config: *appConfig,
			BypassMinFeeMsgTypes: []string{
				// todo: use ibc as exmaple ?
				sdk.MsgTypeURL(&ibcchanneltypes.MsgRecvPacket{}),
				sdk.MsgTypeURL(&ibcchanneltypes.MsgAcknowledgement{}),
				sdk.MsgTypeURL(&ibcclienttypes.MsgUpdateClient{}),
				"/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward",
			},
		}

		customAppTemplate := `
###############################################################################
###                        Custom Gaia Configuration                        ###
###############################################################################
# bypass-min-fee-msg-types defines custom message types the operator may set that
# will bypass minimum fee checks during CheckTx.
#
# Example:
# ["/ibc.core.channel.v1.MsgRecvPacket", "/ibc.core.channel.v1.MsgAcknowledgement", ...]
bypass-min-fee-msg-types = ["/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward","/ibc.applications.transfer.v1.MsgTransfer"]
` + srvconfig.DefaultConfigTemplate
		srvconfig.SetConfigTemplate(customAppTemplate)
		srvconfig.WriteConfigFile(appCfgPath, appCustomConfig)
	}
}

// runValidators runs the validators in the chain
func (s *IntegrationTestSuite) runValidators(c *chain, portOffset int) {
	s.T().Logf("starting Gaia %s validator containers...", c.id)

	s.valResources[c.id] = make([]*dockertest.Resource, len(c.validators))
	for i, val := range c.validators {
		runOpts := &dockertest.RunOptions{
			Name:      val.instanceName(),
			NetworkID: s.dkrNet.Network.ID,
			Mounts: []string{
				fmt.Sprintf("%s/:/home/nonroot/.gaia", val.configDir()),
			},
			Repository: "cosmos/gaiad-e2e",
		}

		s.Require().NoError(exec.Command("chmod", "-R", "0777", val.configDir()).Run())

		// expose the first validator for debugging and communication
		if val.index == 0 {
			runOpts.PortBindings = map[docker.Port][]docker.PortBinding{
				"1317/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 1317+portOffset)}},
				"6060/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6060+portOffset)}},
				"6061/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6061+portOffset)}},
				"6062/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6062+portOffset)}},
				"6063/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6063+portOffset)}},
				"6064/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6064+portOffset)}},
				"6065/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6065+portOffset)}},
				"9090/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 9090+portOffset)}},
				"26656/tcp": {{HostIP: "", HostPort: fmt.Sprintf("%d", 26656+portOffset)}},
				"26657/tcp": {{HostIP: "", HostPort: fmt.Sprintf("%d", 26657+portOffset)}},
			}
		}

		resource, err := s.dkrPool.RunWithOptions(runOpts, noRestart)
		s.Require().NoError(err)

		s.valResources[c.id][i] = resource
		s.T().Logf("started Gaia %s validator container: %s", c.id, resource.Container.ID)
	}

	rpcClient, err := rpchttp.New("tcp://localhost:26657", "/websocket")
	s.Require().NoError(err)

	s.Require().Eventually(
		func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			defer cancel()

			status, err := rpcClient.Status(ctx)
			if err != nil {
				return false
			}

			// let the node produce a few blocks
			if status.SyncInfo.CatchingUp || status.SyncInfo.LatestBlockHeight < 3 {
				return false
			}

			return true
		},
		5*time.Minute,
		time.Second,
		"Gaia node failed to produce blocks",
	)
}

func (s *IntegrationTestSuite) runIBCRelayer() {
	s.T().Log("starting Hermes relayer container...")

	tmpDir, err := ioutil.TempDir("", "gaia-e2e-testnet-hermes-")
	s.Require().NoError(err)
	s.tmpDirs = append(s.tmpDirs, tmpDir)

	gaiaAVal := s.chainA.validators[0]
	gaiaBVal := s.chainB.validators[0]

	gaiaARly := s.chainA.genesisAccounts[relayerAccountIndex]
	gaiaBRly := s.chainB.genesisAccounts[relayerAccountIndex]

	hermesCfgPath := path.Join(tmpDir, "hermes")

	s.Require().NoError(os.MkdirAll(hermesCfgPath, 0o755))
	_, err = copyFile(
		filepath.Join("./scripts/", "hermes_bootstrap.sh"),
		filepath.Join(hermesCfgPath, "hermes_bootstrap.sh"),
	)
	s.Require().NoError(err)

	s.hermesResource, err = s.dkrPool.RunWithOptions(
		&dockertest.RunOptions{
			Name:       fmt.Sprintf("%s-%s-relayer", s.chainA.id, s.chainB.id),
			Repository: "ghcr.io/cosmos/hermes-e2e",
			Tag:        "1.0.0",
			NetworkID:  s.dkrNet.Network.ID,
			Mounts: []string{
				fmt.Sprintf("%s/:/root/hermes", hermesCfgPath),
			},
			PortBindings: map[docker.Port][]docker.PortBinding{
				"3031/tcp": {{HostIP: "", HostPort: "3031"}},
			},
			Env: []string{
				fmt.Sprintf("GAIA_A_E2E_CHAIN_ID=%s", s.chainA.id),
				fmt.Sprintf("GAIA_B_E2E_CHAIN_ID=%s", s.chainB.id),
				fmt.Sprintf("GAIA_A_E2E_VAL_MNEMONIC=%s", gaiaAVal.mnemonic),
				fmt.Sprintf("GAIA_B_E2E_VAL_MNEMONIC=%s", gaiaBVal.mnemonic),
				fmt.Sprintf("GAIA_A_E2E_RLY_MNEMONIC=%s", gaiaARly.mnemonic),
				fmt.Sprintf("GAIA_B_E2E_RLY_MNEMONIC=%s", gaiaBRly.mnemonic),
				fmt.Sprintf("GAIA_A_E2E_VAL_HOST=%s", s.valResources[s.chainA.id][0].Container.Name[1:]),
				fmt.Sprintf("GAIA_B_E2E_VAL_HOST=%s", s.valResources[s.chainB.id][0].Container.Name[1:]),
			},
			Entrypoint: []string{
				"sh",
				"-c",
				"chmod +x /root/hermes/hermes_bootstrap.sh && /root/hermes/hermes_bootstrap.sh",
			},
		},
		noRestart,
	)
	s.Require().NoError(err)

	endpoint := fmt.Sprintf("http://%s/state", s.hermesResource.GetHostPort("3031/tcp"))
	s.Require().Eventually(
		func() bool {
			resp, err := http.Get(endpoint)
			if err != nil {
				return false
			}

			defer resp.Body.Close()

			bz, err := io.ReadAll(resp.Body)
			if err != nil {
				return false
			}

			var respBody map[string]interface{}
			if err := json.Unmarshal(bz, &respBody); err != nil {
				return false
			}

			status := respBody["status"].(string)
			result := respBody["result"].(map[string]interface{})

			return status == "success" && len(result["chains"].([]interface{})) == 2
		},
		5*time.Minute,
		time.Second,
		"hermes relayer not healthy",
	)

	s.T().Logf("started Hermes relayer container: %s", s.hermesResource.Container.ID)

	// XXX: Give time to both networks to start, otherwise we might see gRPC
	// transport errors.
	time.Sleep(10 * time.Second)

	// create the client, connection and channel between the two Gaia chains
	s.createConnection()
	time.Sleep(10 * time.Second)
	s.createChannel()
}

func noRestart(config *docker.HostConfig) {
	// in this case we don't want the nodes to restart on failure
	config.RestartPolicy = docker.RestartPolicy{
		Name: "no",
	}
}

func (s *IntegrationTestSuite) writeGovProposals(c *chain) {
	type GovMessageSend struct {
		Type   string     `json:"@type"`
		From   string     `json:"from_address"`
		To     string     `json:"to_address"`
		Amount []sdk.Coin `json:"amount"`
	}

	msgSendMessages := []GovMessageSend{
		{
			Type:   "/cosmos.bank.v1beta1.MsgSend",
			From:   govModuleAddress,
			To:     govSendMsgRecipientAddress,
			Amount: []sdk.Coin{sendGovAmount},
		},
	}

	msgSendBody, err := json.MarshalIndent(struct {
		Messages []GovMessageSend `json:"messages"`
		Metadata string           `json:"metadata"`
		Deposit  string           `json:"deposit"`
	}{
		Messages: msgSendMessages,
		Metadata: b64.StdEncoding.EncodeToString([]byte("Testing 1, 2, 3!")),
		Deposit:  "5000uatom",
	}, "", " ")

	s.Require().NoError(err)

	legacyCommunitySpendBody, err := json.MarshalIndent(struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Recipient   string `json:"recipient"`
		Amount      string `json:"amount"`
		Deposit     string `json:"deposit"`
	}{
		Title:       "Community Pool Spend",
		Description: "Fund Gov !",
		Recipient:   govModuleAddress,
		Amount:      "1000uatom",
		Deposit:     "5000uatom",
	}, "", " ")

	s.Require().NoError(err)

	for _, val := range c.validators {
		err = writeFile(filepath.Join(val.configDir(), "config", "proposal.json"), legacyCommunitySpendBody)
		s.Require().NoError(err)

		err = writeFile(filepath.Join(val.configDir(), "config", "proposal_2.json"), msgSendBody)
		s.Require().NoError(err)
	}
}

func (s *IntegrationTestSuite) writeGovUpgradeSoftwareProposal(c *chain, height int) {
	softwareUpgradeMessages := []SoftwareUpgrade{
		{
			Type:      "/cosmos.upgrade.v1beta1.MsgSoftwareUpgrade",
			Authority: govModuleAddress,
			Plan: UpgradePlan{
				Name:   "upgrade-1",
				Height: height,
				Info:   "binary-1",
			},
		},
	}
	cancelSoftwareUpgradeMessages := []CancelSoftwareUpgrade{
		{
			Type:      "/cosmos.upgrade.v1beta1.MsgCancelUpgrade",
			Authority: govModuleAddress,
		},
	}

	upgradeProposalBody, err := json.MarshalIndent(struct {
		Messages []SoftwareUpgrade `json:"messages"`
		Metadata string            `json:"metadata"`
		Deposit  string            `json:"deposit"`
	}{
		Messages: softwareUpgradeMessages,
		Metadata: b64.StdEncoding.EncodeToString([]byte("Testing 1, 2, 3!")),
		Deposit:  "5000uatom",
	}, "", " ")

	cancelUpgradeProposalBody, err := json.MarshalIndent(struct {
		Messages []CancelSoftwareUpgrade `json:"messages"`
		Metadata string                  `json:"metadata"`
		Deposit  string                  `json:"deposit"`
	}{
		Messages: cancelSoftwareUpgradeMessages,
		Metadata: "VGVzdGluZyAxLCAyLCAzIQ==",
		Deposit:  "5000uatom",
	}, "", " ")

	err = writeFile(filepath.Join(c.validators[0].configDir(), "config", "proposal_3.json"), upgradeProposalBody)
	s.Require().NoError(err)

	err = writeFile(filepath.Join(c.validators[0].configDir(), "config", "proposal_4.json"), cancelUpgradeProposalBody)
	s.Require().NoError(err)
}

func (s *IntegrationTestSuite) writeGroupMembers(c *chain, groupMembers []GroupMember, filename string) {
	groupMembersBody, err := json.MarshalIndent(struct {
		Members []GroupMember `json:"members"`
	}{
		Members: groupMembers,
	}, "", " ")
	s.Require().NoError(err)

	s.writeFile(c, filename, groupMembersBody)
}

func (s *IntegrationTestSuite) writeFile(c *chain, filename string, body []byte) {
	for _, val := range c.validators {
		err := writeFile(filepath.Join(val.configDir(), "config", filename), body)
		s.Require().NoError(err)
	}
}

func (s *IntegrationTestSuite) writeGovParamChangeProposalGlobalFees(c *chain, coins sdk.DecCoins) {
	type ParamInfo struct {
		Subspace string       `json:"subspace"`
		Key      string       `json:"key"`
		Value    sdk.DecCoins `json:"value"`
	}

	type ParamChangeMessage struct {
		Title       string      `json:"title"`
		Description string      `json:"description"`
		Changes     []ParamInfo `json:"changes"`
		Deposit     string      `json:"deposit"`
	}

	paramChangeProposalBody, err := json.MarshalIndent(ParamChangeMessage{
		Title:       "global fee test",
		Description: "global fee change",
		Changes: []ParamInfo{
			{
				Subspace: "globalfee",
				Key:      "MinimumGasPricesParam",
				Value:    coins,
			},
		},
		Deposit: "",
	}, "", " ")
	s.Require().NoError(err)

	err = writeFile(filepath.Join(c.validators[0].configDir(), "config", "proposal_globalfee.json"), paramChangeProposalBody)
	s.Require().NoError(err)
}

func (s *IntegrationTestSuite) writeICAtx(cmd []string, path string) {

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cmd = append(cmd, fmt.Sprintf("--%s=%s", flags.FlagGenerateOnly, "true"))
	s.T().Logf("dry run: ica tx %s", strings.Join(cmd, " "))

	var (
		outBuf bytes.Buffer
		errBuf bytes.Buffer
	)

	type txResponse struct {
		Body struct {
			Messages []map[string]interface{}
		}
	}
	var txResp txResponse

	exe, err := s.dkrPool.Client.CreateExec(docker.CreateExecOptions{
		Context:      ctx,
		AttachStdout: true,
		AttachStderr: true,
		Container:    s.valResources[s.chainA.id][0].Container.ID,
		User:         "nonroot",
		Cmd:          cmd,
	})
	s.Require().NoError(err)

	err = s.dkrPool.Client.StartExec(exe.ID, docker.StartExecOptions{
		Context:      ctx,
		Detach:       false,
		OutputStream: &outBuf,
		ErrorStream:  &errBuf,
	})
	s.Require().NoError(err)

	s.Require().NoError(json.Unmarshal(outBuf.Bytes(), &txResp))
	b, err := json.MarshalIndent(txResp.Body.Messages[0], "", " ")
	s.Require().NoError(err)

	err = writeFile(path, b)
	s.Require().NoError(err)

	s.T().Logf("write ica transaction json to %s", path)
}
