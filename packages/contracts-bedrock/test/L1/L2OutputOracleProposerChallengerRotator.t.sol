// SPDX-License-Identifier: MIT
pragma solidity 0.8.15;

import {CommonTest} from "test/setup/CommonTest.sol";
import {EIP1967Helper} from "test/mocks/EIP1967Helper.sol";
import {L2OutputOracle} from "src/L1/L2OutputOracle.sol";
import {L2OutputOracleProposerChallengerRotator} from "src/L1/L2OutputOracleProposerChallengerRotator.sol";
import {ProxyAdmin} from "src/universal/ProxyAdmin.sol";
import {Vm} from "forge-std/Vm.sol";

contract L2OutputOracleProposerChallengerRotator_Test is CommonTest {
    L2OutputOracleProposerChallengerRotator internal rotator;
    ProxyAdmin internal proxyAdmin;
    address internal originalImplementation;

    function setUp() public override {
        super.setUp();

        rotator = new L2OutputOracleProposerChallengerRotator();
        proxyAdmin = ProxyAdmin(deploy.mustGetAddress("ProxyAdmin"));
        originalImplementation = EIP1967Helper.getImplementation(address(l2OutputOracle));
    }

    function test_rotateProposerAndChallenger_onlyOldProposer_succeeds() external {
        address oldProposer = l2OutputOracle.proposer();
        address oldChallenger = l2OutputOracle.challenger();
        address newProposer = makeAddr("newProposer");
        address newChallenger = makeAddr("newChallenger");

        uint256 startingBlockNumber = l2OutputOracle.startingBlockNumber();
        uint256 startingTimestamp = l2OutputOracle.startingTimestamp();
        uint256 submissionInterval = l2OutputOracle.submissionInterval();
        uint256 l2BlockTime = l2OutputOracle.l2BlockTime();
        uint256 finalizationPeriodSeconds = l2OutputOracle.finalizationPeriodSeconds();
        uint256 latestBlockNumber = l2OutputOracle.latestBlockNumber();
        uint256 nextOutputIndex = l2OutputOracle.nextOutputIndex();

        vm.prank(proxyAdmin.owner());
        proxyAdmin.upgrade(payable(address(l2OutputOracle)), address(rotator));

        vm.recordLogs();
        vm.prank(oldProposer);
        L2OutputOracleProposerChallengerRotator(address(l2OutputOracle))
            .rotateProposerAndChallenger({_proposer: newProposer, _challenger: newChallenger});
        Vm.Log[] memory entries = vm.getRecordedLogs();

        assertEq(entries.length, 1);
        assertEq(entries[0].emitter, address(l2OutputOracle));
        assertEq(entries[0].topics.length, 1);
        assertEq(entries[0].topics[0], keccak256("ProposerAndChallengerRotated(address,address,address,address)"));
        assertEq(entries[0].data, abi.encode(oldProposer, oldChallenger, newProposer, newChallenger));

        assertEq(l2OutputOracle.proposer(), newProposer);
        assertEq(l2OutputOracle.challenger(), newChallenger);
        assertEq(l2OutputOracle.startingBlockNumber(), startingBlockNumber);
        assertEq(l2OutputOracle.startingTimestamp(), startingTimestamp);
        assertEq(l2OutputOracle.submissionInterval(), submissionInterval);
        assertEq(l2OutputOracle.l2BlockTime(), l2BlockTime);
        assertEq(l2OutputOracle.finalizationPeriodSeconds(), finalizationPeriodSeconds);
        assertEq(l2OutputOracle.latestBlockNumber(), latestBlockNumber);
        assertEq(l2OutputOracle.nextOutputIndex(), nextOutputIndex);
        assertTrue(oldProposer != l2OutputOracle.proposer());
        assertTrue(oldChallenger != l2OutputOracle.challenger());

        vm.prank(oldProposer);
        vm.expectRevert("L2OutputOracle: only old proposer can rotate");
        L2OutputOracleProposerChallengerRotator(address(l2OutputOracle))
            .rotateProposerAndChallenger({
                _proposer: makeAddr("secondProposer"), _challenger: makeAddr("secondChallenger")
            });

        vm.prank(proxyAdmin.owner());
        proxyAdmin.upgrade(payable(address(l2OutputOracle)), originalImplementation);

        assertEq(EIP1967Helper.getImplementation(address(l2OutputOracle)), originalImplementation);
        assertEq(l2OutputOracle.proposer(), newProposer);
        assertEq(l2OutputOracle.challenger(), newChallenger);
    }

    function test_rotateProposerAndChallenger_notOldProposer_reverts() external {
        vm.prank(proxyAdmin.owner());
        proxyAdmin.upgrade(payable(address(l2OutputOracle)), address(rotator));

        vm.prank(makeAddr("notOldProposer"));
        vm.expectRevert("L2OutputOracle: only old proposer can rotate");
        L2OutputOracleProposerChallengerRotator(address(l2OutputOracle))
            .rotateProposerAndChallenger({_proposer: makeAddr("newProposer"), _challenger: makeAddr("newChallenger")});
    }

    function test_rotateProposerAndChallenger_zeroAddress_reverts() external {
        address oldProposer = l2OutputOracle.proposer();

        vm.prank(proxyAdmin.owner());
        proxyAdmin.upgrade(payable(address(l2OutputOracle)), address(rotator));

        vm.prank(oldProposer);
        vm.expectRevert("L2OutputOracle: proposer cannot be zero address");
        L2OutputOracleProposerChallengerRotator(address(l2OutputOracle))
            .rotateProposerAndChallenger({_proposer: address(0), _challenger: makeAddr("newChallenger")});

        vm.prank(oldProposer);
        vm.expectRevert("L2OutputOracle: challenger cannot be zero address");
        L2OutputOracleProposerChallengerRotator(address(l2OutputOracle))
            .rotateProposerAndChallenger({_proposer: makeAddr("newProposer"), _challenger: address(0)});
    }
}
