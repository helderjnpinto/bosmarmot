package jobs

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyperledger/burrow/crypto"
	"github.com/hyperledger/burrow/txs/payload"
	"github.com/monax/bosmarmot/bos/abi"
	compilers "github.com/monax/bosmarmot/bos/compile"
	"github.com/monax/bosmarmot/bos/def"
	"github.com/monax/bosmarmot/bos/util"
	log "github.com/sirupsen/logrus"
)

func DeployJob(deploy *def.Deploy, do *def.Packages) (result string, err error) {
	// Preprocess variables
	deploy.Source, _ = util.PreProcess(deploy.Source, do)
	deploy.Contract, _ = util.PreProcess(deploy.Contract, do)
	deploy.Instance, _ = util.PreProcess(deploy.Instance, do)
	deploy.Libraries, _ = util.PreProcessLibs(deploy.Libraries, do)
	deploy.Amount, _ = util.PreProcess(deploy.Amount, do)
	deploy.Sequence, _ = util.PreProcess(deploy.Sequence, do)
	deploy.Fee, _ = util.PreProcess(deploy.Fee, do)
	deploy.Gas, _ = util.PreProcess(deploy.Gas, do)

	// trim the extension
	contractName := strings.TrimSuffix(deploy.Contract, filepath.Ext(deploy.Contract))

	// Use defaults
	deploy.Source = useDefault(deploy.Source, do.Package.Account)
	deploy.Instance = useDefault(deploy.Instance, contractName)
	deploy.Amount = useDefault(deploy.Amount, do.DefaultAmount)
	deploy.Fee = useDefault(deploy.Fee, do.DefaultFee)
	deploy.Gas = useDefault(deploy.Gas, do.DefaultGas)

	// assemble contract
	var contractPath string
	if _, err := os.Stat(deploy.Contract); err != nil {
		if _, secErr := os.Stat(filepath.Join(do.BinPath, deploy.Contract)); secErr != nil {
			if _, thirdErr := os.Stat(filepath.Join(do.BinPath, filepath.Base(deploy.Contract))); thirdErr != nil {
				return "", fmt.Errorf("Could not find contract in\n* primary path: %v\n* binary path: %v\n* tertiary path: %v", deploy.Contract, filepath.Join(do.BinPath, deploy.Contract), filepath.Join(do.BinPath, filepath.Base(deploy.Contract)))
			} else {
				contractPath = filepath.Join(do.BinPath, filepath.Base(deploy.Contract))
			}
		} else {
			contractPath = filepath.Join(do.BinPath, deploy.Contract)
		}
	} else {
		contractPath = deploy.Contract
	}

	// compile
	if filepath.Ext(deploy.Contract) == ".bin" {
		log.Info("Binary file detected. Using binary deploy sequence.")
		log.WithField("=>", contractPath).Info("Binary path")
		binaryResponse, err := compilers.RequestBinaryLinkage(contractPath, deploy.Libraries)
		if err != nil {
			return "", fmt.Errorf("Something went wrong with your binary deployment: %v", err)
		}
		if binaryResponse.Error != "" {
			return "", fmt.Errorf("Something went wrong when you were trying to link your binaries: %v", binaryResponse.Error)
		}
		contractCode := binaryResponse.Binary

		tx, err := deployTx(do, deploy, contractName, string(contractCode))
		if err != nil {
			return "could not deploy binary contract", err
		}
		result, err := deployFinalize(do, tx)
		if err != nil {
			return "", fmt.Errorf("Error finalizing contract deploy from path %s: %v", contractPath, err)
		}
		return result.String(), err
	} else {
		contractPath = deploy.Contract
		log.WithField("=>", contractPath).Info("Contract path")
		// normal compilation/deploy sequence
		resp, err := compilers.RequestCompile(contractPath, false, deploy.Libraries)

		if err != nil {
			log.Errorln("Error compiling contracts: Compilers error:")
			return "", err
		} else if resp.Error != "" {
			log.Errorln("Error compiling contracts: Language error:")
			return "", fmt.Errorf("%v", resp.Error)
		} else if resp.Warning != "" {
			log.WithField("=>", resp.Warning).Warn("Warning during contract compilation")
		}
		// loop through objects returned from compiler
		switch {
		case len(resp.Objects) == 1:
			log.WithField("path", contractPath).Info("Deploying the only contract in file")
			response := resp.Objects[0]
			log.WithField("=>", response.ABI).Info("Abi")
			log.WithField("=>", response.Bytecode).Info("Bin")
			if response.Bytecode != "" {
				result, err = deployContract(deploy, do, response)
				if err != nil {
					return "", err
				}
			}
		case deploy.Instance == "all":
			log.WithField("path", contractPath).Info("Deploying all contracts")
			var baseObj string
			for _, response := range resp.Objects {
				if response.Bytecode == "" {
					continue
				}
				result, err = deployContract(deploy, do, response)
				if err != nil {
					return "", err
				}
				if strings.ToLower(response.Objectname) == strings.ToLower(strings.TrimSuffix(filepath.Base(deploy.Contract), filepath.Ext(filepath.Base(deploy.Contract)))) {
					baseObj = result
				}
			}
			if baseObj != "" {
				result = baseObj
			}
		default:
			log.WithField("contract", deploy.Instance).Info("Deploying a single contract")
			for _, response := range resp.Objects {
				if response.Bytecode == "" {
					continue
				}
				if matchInstanceName(response.Objectname, deploy.Instance) {
					result, err = deployContract(deploy, do, response)
					if err != nil {
						return "", err
					}
				}
			}
		}
	}

	return result, nil
}

func matchInstanceName(objectName, deployInstance string) bool {
	if objectName == "" {
		return false
	}
	// Ignore the filename component that newer versions of Solidity include in object name

	objectNameParts := strings.Split(objectName, ":")
	return strings.ToLower(objectNameParts[len(objectNameParts)-1]) == strings.ToLower(deployInstance)
}

// TODO [rj] refactor to remove [contractPath] from functions signature => only used in a single error throw.
func deployContract(deploy *def.Deploy, do *def.Packages, compilersResponse compilers.ResponseItem) (string, error) {
	log.WithField("=>", string(compilersResponse.ABI)).Debug("ABI Specification (From Compilers)")
	contractCode := compilersResponse.Bytecode

	// Save ABI
	if _, err := os.Stat(do.ABIPath); os.IsNotExist(err) {
		if err := os.Mkdir(do.ABIPath, 0775); err != nil {
			return "", err
		}
	}
	if _, err := os.Stat(do.BinPath); os.IsNotExist(err) {
		if err := os.Mkdir(do.BinPath, 0775); err != nil {
			return "", err
		}
	}

	// saving contract/library abi
	var abiLocation string
	if compilersResponse.Objectname != "" {
		abiLocation = filepath.Join(do.ABIPath, compilersResponse.Objectname)
		log.WithField("=>", abiLocation).Warn("Saving ABI")
		if err := ioutil.WriteFile(abiLocation, []byte(compilersResponse.ABI), 0664); err != nil {
			return "", err
		}
	} else {
		log.Debug("Objectname from compilers is blank. Not saving abi.")
	}

	// additional data may be sent along with the contract
	// these are naively added to the end of the contract code using standard
	// mint packing

	if deploy.Data != nil {
		_, callDataArray, err := util.PreProcessInputData(compilersResponse.Objectname, deploy.Data, do, true)
		if err != nil {
			return "", err
		}
		packedBytes, err := abi.ReadAbiFormulateCall(compilersResponse.Objectname, "", callDataArray, do)
		if err != nil {
			return "", err
		}
		callData := hex.EncodeToString(packedBytes)
		contractCode = contractCode + callData
	}

	tx, err := deployTx(do, deploy, compilersResponse.Objectname, contractCode)
	if err != nil {
		return "", err
	}

	// Sign, broadcast, display
	contractAddress, err := deployFinalize(do, tx)
	if err != nil {
		return "", fmt.Errorf("Error finalizing contract deploy %s: %v", deploy.Contract, err)
	}

	// saving contract/library abi at abi/address
	if contractAddress != nil {
		abiLocation := filepath.Join(do.ABIPath, contractAddress.String())
		log.WithField("=>", abiLocation).Debug("Saving ABI")
		if err := ioutil.WriteFile(abiLocation, []byte(compilersResponse.ABI), 0664); err != nil {
			return "", err
		}
		// saving binary
		if deploy.SaveBinary {
			contractName := filepath.Join(do.BinPath, fmt.Sprintf("%s.bin", compilersResponse.Objectname))
			log.WithField("=>", contractName).Warn("Saving Binary")
			if err := ioutil.WriteFile(contractName, []byte(contractCode), 0664); err != nil {
				return "", err
			}
		} else {
			log.Debug("Not saving binary.")
		}
		return contractAddress.String(), nil
	} else {
		// we shouldn't reach this point because we should have an error before this.
		return "", fmt.Errorf("The contract did not deploy. Unable to save abi to abi/contractAddress.")
	}
}

func deployTx(do *def.Packages, deploy *def.Deploy, contractName, contractCode string) (*payload.CallTx, error) {
	// Deploy contract
	log.WithFields(log.Fields{
		"name": contractName,
	}).Warn("Deploying Contract")

	log.WithFields(log.Fields{
		"source":    deploy.Source,
		"code":      contractCode,
		"chain-url": do.ChainURL,
	}).Info()

	return do.Call(def.CallArg{
		Input:    deploy.Source,
		Amount:   deploy.Amount,
		Fee:      deploy.Fee,
		Gas:      deploy.Gas,
		Data:     contractCode,
		Sequence: deploy.Sequence,
	})
}

func CallJob(call *def.Call, do *def.Packages) (string, []*def.Variable, error) {
	var err error
	var callData string
	var callDataArray []string
	// Preprocess variables
	call.Source, _ = util.PreProcess(call.Source, do)
	call.Destination, _ = util.PreProcess(call.Destination, do)
	//todo: find a way to call the fallback function here
	call.Function, callDataArray, err = util.PreProcessInputData(call.Function, call.Data, do, false)
	if err != nil {
		return "", nil, err
	}
	call.Function, _ = util.PreProcess(call.Function, do)
	call.Amount, _ = util.PreProcess(call.Amount, do)
	call.Sequence, _ = util.PreProcess(call.Sequence, do)
	call.Fee, _ = util.PreProcess(call.Fee, do)
	call.Gas, _ = util.PreProcess(call.Gas, do)
	call.ABI, _ = util.PreProcess(call.ABI, do)

	// Use default
	call.Source = useDefault(call.Source, do.Package.Account)
	call.Amount = useDefault(call.Amount, do.DefaultAmount)
	call.Fee = useDefault(call.Fee, do.DefaultFee)
	call.Gas = useDefault(call.Gas, do.DefaultGas)

	// formulate call
	var packedBytes []byte
	if call.ABI == "" {
		packedBytes, err = abi.ReadAbiFormulateCall(call.Destination, call.Function, callDataArray, do)
		callData = hex.EncodeToString(packedBytes)
	} else {
		packedBytes, err = abi.ReadAbiFormulateCall(call.ABI, call.Function, callDataArray, do)
		callData = hex.EncodeToString(packedBytes)
	}
	if err != nil {
		if call.Function == "()" {
			log.Warn("Calling the fallback function")
		} else {
			var str, err = util.ABIErrorHandler(do, err, call, nil)
			return str, nil, err
		}
	}

	log.WithFields(log.Fields{
		"destination": call.Destination,
		"function":    call.Function,
		"data":        callData,
	}).Info("Calling")

	tx, err := do.Call(def.CallArg{
		Input:    call.Source,
		Amount:   call.Amount,
		Address:  call.Destination,
		Fee:      call.Fee,
		Gas:      call.Gas,
		Data:     callData,
		Sequence: call.Sequence,
	})
	if err != nil {
		return "", nil, err
	}

	// Sign, broadcast, display
	txe, err := do.SignAndBroadcast(tx)
	if err != nil {
		var err = util.MintChainErrorHandler(do, err)
		return "", nil, err
	}

	var result string
	log.Debug(txe.Result.Return)

	// Formally process the return
	if txe.Result.Return != nil {
		log.WithField("=>", result).Debug("Decoding Raw Result")
		if call.ABI == "" {
			call.Variables, err = abi.ReadAndDecodeContractReturn(call.Destination, call.Function, txe.Result.Return, do)
		} else {
			call.Variables, err = abi.ReadAndDecodeContractReturn(call.ABI, call.Function, txe.Result.Return, do)
		}
		if err != nil {
			return "", nil, err
		}
		log.WithField("=>", call.Variables).Debug("call variables:")
		result = util.GetReturnValue(call.Variables)
		if result != "" {
			log.WithField("=>", result).Warn("Return Value")
		} else {
			log.Debug("No return.")
		}
	} else {
		log.Debug("No return from contract.")
	}

	if call.Save == "tx" {
		log.Info("Saving tx hash instead of contract return")
		result = fmt.Sprintf("%X", txe.Receipt.TxHash)
	}

	return result, call.Variables, nil
}

func deployFinalize(do *def.Packages, tx payload.Payload) (*crypto.Address, error) {
	txe, err := do.SignAndBroadcast(tx)
	if err != nil {
		return nil, util.MintChainErrorHandler(do, err)
	}

	if err := util.ReadTxSignAndBroadcast(txe, err); err != nil {
		return nil, err
	}

	if !txe.Receipt.CreatesContract || txe.Receipt.ContractAddress == crypto.ZeroAddress {
		// Shouldn't get ZeroAddress when CreatesContract is true, but still
		return nil, fmt.Errorf("result from SignAndBroadcast does not contain address for the deployed contract")
	}
	return &txe.Receipt.ContractAddress, nil
}
