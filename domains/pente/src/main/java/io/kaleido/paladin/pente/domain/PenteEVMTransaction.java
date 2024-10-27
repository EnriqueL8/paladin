/*
 * Copyright © 2024 Kaleido, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on
 * an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package io.kaleido.paladin.pente.domain;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.annotation.JsonSerialize;
import com.fasterxml.jackson.databind.ser.std.ToStringSerializer;
import com.google.protobuf.ByteString;
import io.kaleido.paladin.pente.evmrunner.EVMRunner;
import io.kaleido.paladin.pente.evmrunner.EVMVersion;
import io.kaleido.paladin.pente.evmstate.AccountLoader;
import io.kaleido.paladin.pente.evmstate.DynamicLoadWorldState;
import io.kaleido.paladin.pente.evmstate.PersistedAccount;
import io.kaleido.paladin.toolkit.*;
import io.kaleido.paladin.toolkit.JsonHex.Address;
import org.apache.logging.log4j.LogManager;
import org.apache.logging.log4j.Logger;
import org.apache.tuweni.bytes.Bytes;
import org.hyperledger.besu.evm.frame.MessageFrame;
import org.hyperledger.besu.evm.internal.EvmConfiguration;
import org.hyperledger.besu.evm.log.Log;

import java.io.IOException;
import java.math.BigInteger;
import java.util.*;
import java.util.concurrent.ExecutionException;

// This is the data structure
@JsonIgnoreProperties(ignoreUnknown = true)
class PenteEVMTransaction {

    private static final Logger LOGGER = LogManager.getLogger(PenteEVMTransaction.class);

    @JsonProperty
    Address from;

    @JsonProperty
    Address to;

    @JsonProperty
    JsonHex.Uint256 nonce;

    @JsonProperty
    JsonHex.Uint256 gas;

    @JsonProperty
    JsonHex.Uint256 value;

    @JsonProperty
    JsonHex.Bytes data;


    /** the domain, required for chainId config */
    PenteDomain domain;

    /** the EVM version to execute at */
    String evmVersion;

    /** the base block we pass to the EVM for execution as a virtual block number */
    long baseBlock;

    /** split out from the full encoded data - will have a function selector prefix for functions, but not for deploy */
    byte[] callData;

    /** for deploy only, this is the separated out bytecode ahead of the calldata */
    byte[] bytecode;

    /** because we might have been created via JSON de-serialization, we have a default constructor and have to post-init on that branch */
    boolean initialized = false;

    static class EVMExecutionException extends Exception {
        EVMExecutionException(String message) {
            super(message);
        }
    }

    record EVMExecutionResult(
            EVMRunner evm,
            org.hyperledger.besu.datatypes.Address senderAddress,
            org.hyperledger.besu.datatypes.Address contractAddress,
            List<Log> logs,
            byte[] outputData
    ) { }

    /** default constructor for JSON */
    public PenteEVMTransaction() {}

    /**
     * In assemble and exec-call we construct the Ethereum transaction, without a nonce, from the parameters supplied by the sender
     * of the transaction.
     *
     * The nonce is NOT assigned at this point, instead we will look up the current account state from the chain on this path.
     *
     * @param domain the domain
     * @param ptx the common transaction input request for all functions that have the original transaction input available
     * @param from the sender of the request extracted from the resolved verifiers
     */
    PenteEVMTransaction(PenteDomain domain, PenteTransaction ptx, Address from) throws IOException, ExecutionException, InterruptedException, ClassNotFoundException {
        this.domain = domain;
        this.evmVersion = ptx.getConfig().evmVersion();
        this.baseBlock = ptx.getBaseBlock();

        var values = ptx.getValues();

        this.from = from;
        this.to = values.to();
        this.nonce = null; // to be assigned in invoke based on the current state of the chain
        this.gas = new JsonHex.Uint256(values.gas());
        this.value = new JsonHex.Uint256(values.value());

        this.callData = ptx.getEncodedCallData();
        if (this.to == null) {
            // We need to keep track of the bytecode length so we can split it back out again for execution,
            // but the EVM transaction has the concatenation of the bytecode and call data.
            bytecode = values.bytecode().getBytes();
            var data = new byte[callData.length + bytecode.length];
            System.arraycopy(bytecode, 0, data, 0, bytecode.length);
            System.arraycopy(callData, 0, data, bytecode.length, callData.length);
            this.data = new JsonHex.Bytes(data);
        }
        else {
            this.bytecode = null; // no bytecode on this path
            this.data = new JsonHex.Bytes(callData);
        }

        // This constructor does the init on this path
        initialized = true;
    }

    /**
     * Factory function to restore an EVM transaction from the transaction inputs state generated by the assemble step, and stored as "info" on the transction
     * @param domain the domain
     * @param txInputBytes the JSON serialized bytes of the input state
     * @return EVM Transaction ready to re-execute
     */
    static PenteEVMTransaction buildFromInput(PenteDomain domain, byte[] txInputBytes) throws IOException, ExecutionException, InterruptedException {

        var objectMapper = new ObjectMapper();
        var txInputState = objectMapper.readValue(txInputBytes, PenteTransaction.TransactionInputInfoState.class);

        // Decode the rawTransaction data
        var request = FromDomain.DecodeDataRequest.newBuilder().
                setEncodingType(FromDomain.EncodingType.ETH_TRANSACTION).
                setData(ByteString.copyFrom(txInputState.rawTransaction().getBytes())).
                build();
        var response = domain.decodeData(request).get();

        // JSON parse the result back into our base object
        var evmTxn = objectMapper.readValue(response.getBody(), PenteEVMTransaction.class);

        // Now complete initialization with the other information in the info
        evmTxn.postJSONParseInit(domain, txInputState.evmVersion(), txInputState.baseBlock().longValue(), txInputState.bytecodeLength().intValue());

        return evmTxn;

    }

    private void postJSONParseInit(PenteDomain domain, String evmVersion, long baseBlock, int bytecodeLen) {
        if (initialized) {
            throw new IllegalStateException("EVM transaction already initialized");
        }
        this.domain = domain;
        this.evmVersion = evmVersion;
        this.baseBlock = baseBlock;

        if (to != null) {
            var data = this.data.getBytes();
            if (bytecodeLen > data.length) {
                throw new IllegalStateException("bytecode length cannot exceed data length");
            }
            this.bytecode = new byte[bytecodeLen];
            System.arraycopy(data, 0, this.bytecode, 0, bytecodeLen);
            this.callData = new byte[data.length-bytecodeLen];
            System.arraycopy(data, bytecodeLen, this.callData, 0, this.callData.length);
        } else {
            if (bytecodeLen > 0) {
                throw new IllegalStateException("cannot set bytecode length for an invoke");
            }
            this.bytecode = null;
            this.callData = this.data.getBytes();
        }

        this.initialized = true;
    }

    EVMRunner getEVM(long chainId, long blockNumber, AccountLoader accountLoader) throws ClassNotFoundException {
        var evmConfig = EvmConfiguration.DEFAULT;
        EVMVersion evmVersion = switch (this.evmVersion) {
            case "london" -> EVMVersion.London(chainId, evmConfig);
            case "paris" -> EVMVersion.Paris(chainId, evmConfig);
            case "shanghai" -> EVMVersion.Shanghai(chainId, evmConfig);
            default -> throw new IllegalArgumentException("unknown EVM version '%s'".formatted(this.evmVersion));
        };
        return new EVMRunner(evmVersion, accountLoader, blockNumber);
    }

    EVMExecutionResult invokeEVM(AccountLoader accountLoader) throws IOException, ClassNotFoundException, EVMExecutionException {
        if (!initialized) throw new IllegalArgumentException("transaction has not been initialized");

        var evm = getEVM(domain.getConfig().getChainId(), baseBlock, accountLoader);
        var senderAddress = org.hyperledger.besu.datatypes.Address.wrap(Bytes.wrap(from.getBytes()));
        var sender = evm.getWorld().getUpdater().getOrCreate(senderAddress);
        var senderNonce = sender.getNonce();
        if (this.nonce != null) {
            if (this.nonce.longValue() != senderNonce) {
                throw new EVMExecutionException("nonce wrong for EVM state supplied=%d accountState=%d".formatted(this.nonce.longValue(), senderNonce));
            }
        }

        MessageFrame execResult;
        if (to == null) {
            execResult = evm.runContractDeploymentBytes(
                    senderAddress,
                    null,
                    Bytes.wrap(bytecode),
                    Bytes.wrap(callData)
            );
        } else {
            execResult = evm.runContractInvokeBytes(
                    senderAddress,
                    org.hyperledger.besu.datatypes.Address.wrap(Bytes.wrap(to.getBytes())),
                    Bytes.wrap(callData)
            );
        }
        if (execResult.getState() != MessageFrame.State.COMPLETED_SUCCESS) {
            throw new EVMExecutionException("transaction reverted: %s".formatted(execResult.getRevertReason()));
        }

        // Store the nonce back to the structure if not supplied
        if (this.nonce == null) {
            this.nonce = new JsonHex.Uint256(senderNonce);
        }

        // Note we only increment the nonce after successful executions
        sender.setNonce(senderNonce+1);
        evm.getWorld().getUpdater().commit();
        return new EVMExecutionResult(
                evm,
                senderAddress,
                execResult.getContractAddress(),
                execResult.getLogs(),
                execResult.getOutputData().toArray()
        );
    }

    public Address getFrom() {
        return from;
    }

    public void setFrom(Address from) {
        this.from = from;
    }

    public Address getTo() {
        return to;
    }

    public void setTo(Address to) {
        this.to = to;
    }

    public JsonHex.Uint256 getNonce() {
        return nonce;
    }

    public void setNonce(JsonHex.Uint256 nonce) {
        this.nonce = nonce;
    }

    public JsonHex.Uint256 getGas() {
        return gas;
    }

    public void setGas(JsonHex.Uint256 gas) {
        this.gas = gas;
    }

    public JsonHex.Uint256 getValue() {
        return value;
    }

    public void setValue(JsonHex.Uint256 value) {
        this.value = value;
    }

    public JsonHex.Bytes getData() {
        return data;
    }

    public void setData(JsonHex.Bytes data) {
        this.data = data;
    }

    int getBytecodeLen() {
        if (this.bytecode == null) {
            return 0;
        }
        return this.bytecode.length;
    }

    long getBaseBlock() {
        return this.baseBlock;
    }

    String getEVMVersion() {
        return this.evmVersion;
    }
}
