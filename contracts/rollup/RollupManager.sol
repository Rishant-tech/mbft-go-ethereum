// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title RollupManager
 * @dev Manages registration, compressed batch submission, state commitments, 
 *      and cross-domain asset bridging for multiple Layer 2 rollups on mBFT Layer 1.
 */
contract RollupManager {
    address public owner;

    struct RollupInfo {
        uint256 rollupId;
        string name;
        address sequencer;
        bytes32 latestStateRoot;
        uint256 latestL2Block;
        uint256 totalBatches;
        uint256 depositCount;
        bool active;
    }

    struct BatchRecord {
        bytes32 prevStateRoot;
        bytes32 newStateRoot;
        uint256 l2BlockStart;
        uint256 l2BlockEnd;
        bytes32 batchHash;
        uint256 timestamp;
    }

    // rollupId => RollupInfo
    mapping(uint256 => RollupInfo) public rollups;
    // rollupId => batchIndex => BatchRecord
    mapping(uint256 => mapping(uint256 => BatchRecord)) public batches;
    // rollupId => withdrawalId => processed
    mapping(uint256 => mapping(uint256 => bool)) public processedWithdrawals;
    // rollupId => stateRoot => exists
    mapping(uint256 => mapping(bytes32 => bool)) public validStateRoots;

    // Events
    event RollupRegistered(uint256 indexed rollupId, string name, address indexed sequencer, bytes32 genesisStateRoot);
    event SequencerUpdated(uint256 indexed rollupId, address indexed oldSequencer, address indexed newSequencer);
    event BatchSubmitted(
        uint256 indexed rollupId,
        uint256 indexed batchIndex,
        bytes32 prevStateRoot,
        bytes32 newStateRoot,
        uint256 l2BlockStart,
        uint256 l2BlockEnd,
        bytes32 batchHash,
        uint256 dataLength
    );
    event Deposit(
        uint256 indexed rollupId,
        address indexed sender,
        address indexed recipient,
        uint256 amount,
        uint256 depositId
    );
    event Withdrawal(
        uint256 indexed rollupId,
        address indexed recipient,
        uint256 amount,
        uint256 withdrawalId
    );

    modifier onlyOwner() {
        require(msg.sender == owner, "Only owner permitted");
        _;
    }

    modifier onlySequencer(uint256 rollupId) {
        require(rollups[rollupId].active, "Rollup not active");
        require(msg.sender == rollups[rollupId].sequencer, "Unauthorized sequencer");
        _;
    }

    constructor() {
        owner = msg.sender;
    }

    /**
     * @notice Registers a new Layer 2 rollup in the multi-rollup cluster.
     */
    function registerRollup(
        uint256 rollupId,
        string calldata name,
        address sequencer,
        bytes32 genesisStateRoot
    ) external onlyOwner {
        require(rollupId > 0, "Invalid rollup ID");
        require(!rollups[rollupId].active, "Rollup already registered");
        require(sequencer != address(0), "Invalid sequencer address");

        rollups[rollupId] = RollupInfo({
            rollupId: rollupId,
            name: name,
            sequencer: sequencer,
            latestStateRoot: genesisStateRoot,
            latestL2Block: 0,
            totalBatches: 0,
            depositCount: 0,
            active: true
        });

        validStateRoots[rollupId][genesisStateRoot] = true;
        emit RollupRegistered(rollupId, name, sequencer, genesisStateRoot);
    }

    /**
     * @notice Updates the designated sequencer for a specific rollup.
     */
    function updateSequencer(uint256 rollupId, address newSequencer) external onlyOwner {
        require(rollups[rollupId].active, "Rollup not active");
        require(newSequencer != address(0), "Invalid sequencer");
        address old = rollups[rollupId].sequencer;
        rollups[rollupId].sequencer = newSequencer;
        emit SequencerUpdated(rollupId, old, newSequencer);
    }

    /**
     * @notice Submits a compressed transaction batch and advances the L2 state root.
     * @param rollupId ID of the rollup
     * @param prevStateRoot Previous confirmed state root
     * @param newStateRoot New state root after executing the batch
     * @param l2BlockStart Starting L2 block height in this batch
     * @param l2BlockEnd Ending L2 block height in this batch
     * @param compressedData Snappy/Zlib compressed transaction data
     */
    function submitBatch(
        uint256 rollupId,
        bytes32 prevStateRoot,
        bytes32 newStateRoot,
        uint256 l2BlockStart,
        uint256 l2BlockEnd,
        bytes calldata compressedData
    ) external onlySequencer(rollupId) {
        RollupInfo storage r = rollups[rollupId];
        require(prevStateRoot == r.latestStateRoot, "Invalid previous state root");
        require(l2BlockEnd >= l2BlockStart, "Invalid block range");
        require(newStateRoot != bytes32(0), "New state root cannot be zero");

        uint256 batchIdx = r.totalBatches;
        bytes32 bHash = keccak256(compressedData);

        batches[rollupId][batchIdx] = BatchRecord({
            prevStateRoot: prevStateRoot,
            newStateRoot: newStateRoot,
            l2BlockStart: l2BlockStart,
            l2BlockEnd: l2BlockEnd,
            batchHash: bHash,
            timestamp: block.timestamp
        });

        r.latestStateRoot = newStateRoot;
        r.latestL2Block = l2BlockEnd;
        r.totalBatches = batchIdx + 1;
        validStateRoots[rollupId][newStateRoot] = true;

        emit BatchSubmitted(
            rollupId,
            batchIdx,
            prevStateRoot,
            newStateRoot,
            l2BlockStart,
            l2BlockEnd,
            bHash,
            compressedData.length
        );
    }

    /**
     * @notice Deposits native ETH to be bridged into the designated L2 rollup.
     */
    function deposit(uint256 rollupId, address recipient) external payable {
        require(rollups[rollupId].active, "Rollup not active");
        require(msg.value > 0, "Deposit amount must be > 0");
        require(recipient != address(0), "Invalid recipient");

        RollupInfo storage r = rollups[rollupId];
        uint256 depId = r.depositCount++;

        emit Deposit(rollupId, msg.sender, recipient, msg.value, depId);
    }

    /**
     * @notice Finalizes a withdrawal from L2 back to L1 verified against a confirmed state root.
     * @param rollupId ID of the rollup
     * @param recipient Recipient address on L1
     * @param amount Amount in wei to withdraw
     * @param withdrawalId Unique withdrawal ID from L2
     * @param stateRoot Confirmed L2 state root containing the withdrawal leaf
     * @param merkleProof Inclusion proof for the withdrawal
     */
    function withdraw(
        uint256 rollupId,
        address payable recipient,
        uint256 amount,
        uint256 withdrawalId,
        bytes32 stateRoot,
        bytes32[] calldata merkleProof
    ) external {
        require(validStateRoots[rollupId][stateRoot], "Invalid or unconfirmed state root");
        require(!processedWithdrawals[rollupId][withdrawalId], "Withdrawal already processed");
        require(address(this).balance >= amount, "Insufficient L1 bridge balance");

        // Compute leaf: keccak256(abi.encodePacked(recipient, amount, withdrawalId))
        bytes32 leaf = keccak256(abi.encodePacked(recipient, amount, withdrawalId));
        require(verifyMerkleProof(merkleProof, stateRoot, leaf), "Invalid withdrawal Merkle proof");

        processedWithdrawals[rollupId][withdrawalId] = true;

        (bool success, ) = recipient.call{value: amount}("");
        require(success, "ETH transfer failed");

        emit Withdrawal(rollupId, recipient, amount, withdrawalId);
    }

    /**
     * @dev Verifies a Merkle proof against a root.
     */
    function verifyMerkleProof(
        bytes32[] calldata proof,
        bytes32 root,
        bytes32 leaf
    ) public pure returns (bool) {
        bytes32 computed = leaf;
        for (uint256 i = 0; i < proof.length; i++) {
            bytes32 proofElement = proof[i];
            if (computed <= proofElement) {
                computed = keccak256(abi.encodePacked(computed, proofElement));
            } else {
                computed = keccak256(abi.encodePacked(proofElement, computed));
            }
        }
        return computed == root;
    }

    // View helper methods
    function getRollup(uint256 rollupId) external view returns (RollupInfo memory) {
        return rollups[rollupId];
    }

    function getBatch(uint256 rollupId, uint256 batchIndex) external view returns (BatchRecord memory) {
        return batches[rollupId][batchIndex];
    }
}
