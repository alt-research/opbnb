// SPDX-License-Identifier: MIT
pragma solidity 0.8.15;

import { L2OutputOracle } from "src/L1/L2OutputOracle.sol";

/// @custom:proxied
/// @title L2OutputOracleProposerChallengerRotator
/// @notice Temporary L2OutputOracle implementation used to rotate proposer and challenger.
contract L2OutputOracleProposerChallengerRotator is L2OutputOracle {
    /// @notice Allows only the proposer that is currently stored in the proxy to rotate roles.
    modifier allowOldProposer() {
        require(msg.sender == proposer, "L2OutputOracle: only old proposer can rotate");
        _;
    }

    /// @notice Rotates the proposer and challenger addresses.
    /// @param _proposer New proposer address.
    /// @param _challenger New challenger address.
    function rotateProposerAndChallenger(address _proposer, address _challenger) public allowOldProposer {
        require(_proposer != address(0), "L2OutputOracle: proposer cannot be zero address");
        require(_challenger != address(0), "L2OutputOracle: challenger cannot be zero address");

        proposer = _proposer;
        challenger = _challenger;
    }
}
