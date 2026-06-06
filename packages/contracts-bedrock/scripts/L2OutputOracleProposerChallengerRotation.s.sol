// SPDX-License-Identifier: MIT
pragma solidity 0.8.15;

import { console2 as console } from "forge-std/console2.sol";
import { Script } from "forge-std/Script.sol";
import { L2OutputOracle } from "src/L1/L2OutputOracle.sol";
import { L2OutputOracleProposerChallengerRotator } from "src/L1/L2OutputOracleProposerChallengerRotator.sol";
import { ProxyAdmin } from "src/universal/ProxyAdmin.sol";

/// @title L2OutputOracleProposerChallengerRotation
/// @notice Utilities for the temporary L2OutputOracle proposer/challenger rotation flow.
contract L2OutputOracleProposerChallengerRotation is Script {
    address internal constant MATCHAIN_L2_OUTPUT_ORACLE_PROXY = 0xc18EBF62817A4ec94C5a4f4978E913975baa6Fc1;
    address internal constant MATCHAIN_PROXY_ADMIN = 0x689B77b3564f57d338E6f3E108C60bec937C8510;
    address internal constant MATCHAIN_OLD_IMPL = 0x80f4b9B3bF1A9d08d8961D9C634e4E5B650cF467;

    address internal constant IOST_L2_OUTPUT_ORACLE_PROXY = 0xA2097102164568C155b81EE843b0Bddc20b11c7e;
    address internal constant IOST_PROXY_ADMIN = 0xe0A3cf00c08621A5011F266Fa09B707ebCB26eA1;
    address internal constant IOST_OLD_IMPL = 0x560ECb7967016fdEAeC8f8bE1b84C95398c805d2;

    struct RotationConfig {
        address proxy;
        address proxyAdmin;
        address oldImplementation;
        address temporaryImplementation;
        address newProposer;
        address newChallenger;
    }

    function deployRotator() public {
        vm.startBroadcast();
        L2OutputOracleProposerChallengerRotator rotator = new L2OutputOracleProposerChallengerRotator();
        vm.stopBroadcast();

        console.log("Temporary implementation:", address(rotator));
    }

    function printMatchainCalldata() public view {
        _printCalldata(
            RotationConfig({
                proxy: MATCHAIN_L2_OUTPUT_ORACLE_PROXY,
                proxyAdmin: MATCHAIN_PROXY_ADMIN,
                oldImplementation: MATCHAIN_OLD_IMPL,
                temporaryImplementation: vm.envAddress("TEMP_IMPL"),
                newProposer: vm.envAddress("NEW_PROPOSER"),
                newChallenger: vm.envAddress("NEW_CHALLENGER")
            })
        );
    }

    function printIostCalldata() public view {
        _printCalldata(
            RotationConfig({
                proxy: IOST_L2_OUTPUT_ORACLE_PROXY,
                proxyAdmin: IOST_PROXY_ADMIN,
                oldImplementation: IOST_OLD_IMPL,
                temporaryImplementation: vm.envAddress("TEMP_IMPL"),
                newProposer: vm.envAddress("NEW_PROPOSER"),
                newChallenger: vm.envAddress("NEW_CHALLENGER")
            })
        );
    }

    function simulateMatchain() public {
        _simulate(
            RotationConfig({
                proxy: MATCHAIN_L2_OUTPUT_ORACLE_PROXY,
                proxyAdmin: MATCHAIN_PROXY_ADMIN,
                oldImplementation: MATCHAIN_OLD_IMPL,
                temporaryImplementation: address(new L2OutputOracleProposerChallengerRotator()),
                newProposer: vm.envAddress("NEW_PROPOSER"),
                newChallenger: vm.envAddress("NEW_CHALLENGER")
            })
        );
    }

    function simulateIost() public {
        _simulate(
            RotationConfig({
                proxy: IOST_L2_OUTPUT_ORACLE_PROXY,
                proxyAdmin: IOST_PROXY_ADMIN,
                oldImplementation: IOST_OLD_IMPL,
                temporaryImplementation: address(new L2OutputOracleProposerChallengerRotator()),
                newProposer: vm.envAddress("NEW_PROPOSER"),
                newChallenger: vm.envAddress("NEW_CHALLENGER")
            })
        );
    }

    function _printCalldata(RotationConfig memory _cfg) internal pure {
        _checkConfig(_cfg);

        bytes memory upgradeToTemporaryImpl =
            abi.encodeCall(ProxyAdmin.upgrade, (payable(_cfg.proxy), _cfg.temporaryImplementation));
        bytes memory rotate = abi.encodeCall(
            L2OutputOracleProposerChallengerRotator.rotateProposerAndChallenger, (_cfg.newProposer, _cfg.newChallenger)
        );
        bytes memory upgradeToOldImpl =
            abi.encodeCall(ProxyAdmin.upgrade, (payable(_cfg.proxy), _cfg.oldImplementation));

        console.log("ProxyAdmin:", _cfg.proxyAdmin);
        console.log("L2OutputOracleProxy:", _cfg.proxy);
        console.log("Temporary implementation:", _cfg.temporaryImplementation);
        console.log("Old implementation:", _cfg.oldImplementation);
        console.log("New proposer:", _cfg.newProposer);
        console.log("New challenger:", _cfg.newChallenger);
        console.log("Safe tx 1 to:", _cfg.proxyAdmin);
        console.log("Safe tx 1 value: 0");
        console.log("Safe tx 1 data: ProxyAdmin.upgrade(proxy, temporaryImplementation)");
        console.logBytes(upgradeToTemporaryImpl);
        console.log("Old proposer tx to:", _cfg.proxy);
        console.log("Old proposer tx value: 0");
        console.log("Old proposer tx data: rotateProposerAndChallenger(newProposer, newChallenger)");
        console.logBytes(rotate);
        console.log("Safe tx 2 to:", _cfg.proxyAdmin);
        console.log("Safe tx 2 value: 0");
        console.log("Safe tx 2 data: ProxyAdmin.upgrade(proxy, oldImplementation)");
        console.logBytes(upgradeToOldImpl);
    }

    function _simulate(RotationConfig memory _cfg) internal {
        _checkConfig(_cfg);

        ProxyAdmin proxyAdmin = ProxyAdmin(_cfg.proxyAdmin);
        L2OutputOracle oracle = L2OutputOracle(_cfg.proxy);

        address owner = proxyAdmin.owner();
        address oldProposer = oracle.proposer();
        address oldChallenger = oracle.challenger();
        uint256 startingBlockNumber = oracle.startingBlockNumber();
        uint256 startingTimestamp = oracle.startingTimestamp();
        uint256 submissionInterval = oracle.submissionInterval();
        uint256 l2BlockTime = oracle.l2BlockTime();
        uint256 finalizationPeriodSeconds = oracle.finalizationPeriodSeconds();
        uint256 latestBlockNumber = oracle.latestBlockNumber();
        uint256 nextOutputIndex = oracle.nextOutputIndex();

        require(
            proxyAdmin.getProxyImplementation(_cfg.proxy) == _cfg.oldImplementation, "unexpected old implementation"
        );

        vm.prank(owner);
        proxyAdmin.upgrade(payable(_cfg.proxy), _cfg.temporaryImplementation);
        require(
            proxyAdmin.getProxyImplementation(_cfg.proxy) == _cfg.temporaryImplementation, "temporary upgrade failed"
        );

        vm.prank(oldProposer);
        L2OutputOracleProposerChallengerRotator(_cfg.proxy)
            .rotateProposerAndChallenger({ _proposer: _cfg.newProposer, _challenger: _cfg.newChallenger });

        require(oracle.proposer() == _cfg.newProposer, "proposer not rotated");
        require(oracle.challenger() == _cfg.newChallenger, "challenger not rotated");
        require(oracle.startingBlockNumber() == startingBlockNumber, "startingBlockNumber changed");
        require(oracle.startingTimestamp() == startingTimestamp, "startingTimestamp changed");
        require(oracle.submissionInterval() == submissionInterval, "submissionInterval changed");
        require(oracle.l2BlockTime() == l2BlockTime, "l2BlockTime changed");
        require(oracle.finalizationPeriodSeconds() == finalizationPeriodSeconds, "finalizationPeriodSeconds changed");
        require(oracle.latestBlockNumber() == latestBlockNumber, "latestBlockNumber changed");
        require(oracle.nextOutputIndex() == nextOutputIndex, "nextOutputIndex changed");
        require(oldProposer != oracle.proposer(), "proposer unchanged");
        require(oldChallenger != oracle.challenger(), "challenger unchanged");

        vm.prank(owner);
        proxyAdmin.upgrade(payable(_cfg.proxy), _cfg.oldImplementation);
        require(proxyAdmin.getProxyImplementation(_cfg.proxy) == _cfg.oldImplementation, "rollback upgrade failed");
        require(oracle.proposer() == _cfg.newProposer, "proposer changed after rollback");
        require(oracle.challenger() == _cfg.newChallenger, "challenger changed after rollback");

        console.log("Simulation succeeded");
        console.log("ProxyAdmin owner:", owner);
        console.log("Old proposer:", oldProposer);
        console.log("Old challenger:", oldChallenger);
        console.log("New proposer:", _cfg.newProposer);
        console.log("New challenger:", _cfg.newChallenger);
    }

    function _checkConfig(RotationConfig memory _cfg) internal pure {
        require(_cfg.proxy != address(0), "missing proxy");
        require(_cfg.proxyAdmin != address(0), "missing proxy admin");
        require(_cfg.oldImplementation != address(0), "missing old implementation");
        require(_cfg.temporaryImplementation != address(0), "missing temporary implementation");
        require(_cfg.newProposer != address(0), "missing new proposer");
        require(_cfg.newChallenger != address(0), "missing new challenger");
    }
}
