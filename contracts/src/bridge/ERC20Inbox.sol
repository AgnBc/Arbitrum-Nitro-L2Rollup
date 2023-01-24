// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE
// SPDX-License-Identifier: BUSL-1.1

pragma solidity ^0.8.4;

import {
    AlreadyInit,
    NotAllowedOrigin,
    DataTooLarge,
    AlreadyPaused,
    AlreadyUnpaused,
    Paused,
    InsufficientValue,
    InsufficientSubmissionCost,
    RetryableData,
    NotRollupOrOwner,
    GasLimitTooLarge
} from "../libraries/Error.sol";
import "./AbsInbox.sol";
import "./IERC20Inbox.sol";
import "./IERC20Bridge.sol";
import "./Messages.sol";
import "../libraries/AddressAliasHelper.sol";
import {
    L1MessageType_submitRetryableTx,
    L1MessageType_ethDeposit
} from "../libraries/MessageTypes.sol";
import {MAX_DATA_SIZE} from "../libraries/Constants.sol";
import "@openzeppelin/contracts-upgradeable/utils/AddressUpgradeable.sol";

/**
 * @title Inbox for user and contract originated messages
 * @notice Messages created via this inbox are enqueued in the delayed accumulator
 * to await inclusion in the SequencerInbox
 */
contract ERC20Inbox is AbsInbox, IERC20Inbox {
    /// @inheritdoc IERC20Inbox
    function depositERC20(uint256 amount) public whenNotPaused onlyAllowed returns (uint256) {
        address dest = msg.sender;

        // solhint-disable-next-line avoid-tx-origin
        if (AddressUpgradeable.isContract(msg.sender) || tx.origin != msg.sender) {
            // isContract check fails if this function is called during a contract's constructor.
            dest = AddressAliasHelper.applyL1ToL2Alias(msg.sender);
        }

        return
            _deliverMessage(
                L1MessageType_ethDeposit,
                msg.sender,
                abi.encodePacked(dest, amount),
                amount
            );
    }

    /// @inheritdoc IERC20Inbox
    function createRetryableTicket(
        address to,
        uint256 l2CallValue,
        uint256 maxSubmissionCost,
        address excessFeeRefundAddress,
        address callValueRefundAddress,
        uint256 gasLimit,
        uint256 maxFeePerGas,
        uint256 tokenTotalFeeAmount,
        bytes calldata data
    ) external whenNotPaused onlyAllowed returns (uint256) {
        return
            _createRetryableTicket(
                to,
                l2CallValue,
                maxSubmissionCost,
                excessFeeRefundAddress,
                callValueRefundAddress,
                gasLimit,
                maxFeePerGas,
                tokenTotalFeeAmount,
                data
            );
    }

    /// @inheritdoc IERC20Inbox
    function unsafeCreateRetryableTicket(
        address to,
        uint256 l2CallValue,
        uint256 maxSubmissionCost,
        address excessFeeRefundAddress,
        address callValueRefundAddress,
        uint256 gasLimit,
        uint256 maxFeePerGas,
        uint256 tokenTotalFeeAmount,
        bytes calldata data
    ) public whenNotPaused onlyAllowed returns (uint256) {
        return
            _unsafeCreateRetryableTicket(
                to,
                l2CallValue,
                maxSubmissionCost,
                excessFeeRefundAddress,
                callValueRefundAddress,
                gasLimit,
                maxFeePerGas,
                tokenTotalFeeAmount,
                data
            );
    }

    /// @inheritdoc IInbox
    function calculateRetryableSubmissionFee(uint256, uint256)
        public
        pure
        override(AbsInbox, IInbox)
        returns (uint256)
    {
        return 0;
    }

    function _deliverToBridge(
        uint8 kind,
        address sender,
        bytes32 messageDataHash,
        uint256 tokenAmount
    ) internal override returns (uint256) {
        return
            IERC20Bridge(address(bridge)).enqueueDelayedMessage(
                kind,
                AddressAliasHelper.applyL1ToL2Alias(sender),
                messageDataHash,
                tokenAmount
            );
    }
}
