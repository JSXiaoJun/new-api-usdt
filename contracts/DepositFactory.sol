// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

interface IBalanceOf {
    function balanceOf(address account) external view returns (uint256);
}

/// @notice Implementation behind every deposit wallet clone. A clone holds a
/// user's deposits and can only be emptied by the factory that created it, and
/// the factory only ever empties it into its own destination.
contract DepositForwarder {
    address public immutable factory;

    constructor() {
        factory = msg.sender;
    }

    receive() external payable {}

    /// @dev The transfer's return value is deliberately ignored: USDT on
    /// Ethereum returns nothing and USDT on TRON returns false on success. The
    /// factory proves delivery with the destination's balance instead.
    function flushToken(address token, address to) external returns (uint256 amount) {
        require(msg.sender == factory, "only factory");
        amount = IBalanceOf(token).balanceOf(address(this));
        if (amount == 0) {
            return 0;
        }
        (bool ok, ) = token.call(abi.encodeWithSelector(0xa9059cbb, to, amount));
        require(ok, "token transfer reverted");
    }

    function flushNative(address payable to) external returns (uint256 amount) {
        require(msg.sender == factory, "only factory");
        amount = address(this).balance;
        if (amount == 0) {
            return 0;
        }
        (bool ok, ) = to.call{value: amount}("");
        require(ok, "native transfer failed");
    }
}

/// @notice Issues one CREATE2 deposit wallet per salt and sweeps wallets into
/// a single destination. Sweeping is permissionless on purpose: whoever pays
/// the fee can only move funds to the destination, so the key that pays for
/// sweeps never needs to be trusted.
contract DepositFactory {
    // EIP-1167 minimal proxy, split around the 20-byte implementation address.
    bytes20 private constant CLONE_PREFIX = hex"3d602d80600a3d3981f3363d3d373d3d3d363d73";
    bytes15 private constant CLONE_SUFFIX = hex"5af43d82803e903d91602b57fd5bf3";

    /// @notice 0xff on EVM chains, 0x41 on TRON (TVM prefixes CREATE2 with 0x41).
    bytes1 public immutable create2Prefix;
    address public immutable implementation;

    address public owner;
    address public pendingOwner;
    address public destination;

    event WalletDeployed(bytes32 indexed salt, address indexed wallet);
    event Swept(bytes32 indexed salt, address indexed wallet, address indexed token, uint256 amount);
    event NativeSwept(bytes32 indexed salt, address indexed wallet, uint256 amount);
    event DestinationChanged(address indexed previousDestination, address indexed newDestination);
    event OwnershipTransferStarted(address indexed previousOwner, address indexed newOwner);
    event OwnershipTransferred(address indexed previousOwner, address indexed newOwner);

    modifier onlyOwner() {
        require(msg.sender == owner, "only owner");
        _;
    }

    constructor(address owner_, address destination_, bytes1 create2Prefix_) {
        require(owner_ != address(0), "zero owner");
        require(destination_ != address(0), "zero destination");
        require(create2Prefix_ == 0xff || create2Prefix_ == 0x41, "bad prefix");
        owner = owner_;
        destination = destination_;
        create2Prefix = create2Prefix_;
        implementation = address(new DepositForwarder());
        emit OwnershipTransferred(address(0), owner_);
        emit DestinationChanged(address(0), destination_);
    }

    function cloneCodeHash() public view returns (bytes32) {
        return keccak256(abi.encodePacked(CLONE_PREFIX, implementation, CLONE_SUFFIX));
    }

    function walletAddress(bytes32 salt) public view returns (address) {
        bytes32 digest = keccak256(abi.encodePacked(create2Prefix, address(this), salt, cloneCodeHash()));
        return address(uint160(uint256(digest)));
    }

    /// @notice Deploys the wallet for salt if needed. Reverts when the deployed
    /// address differs from walletAddress(salt), so a wrong CREATE2 formula on
    /// a chain is caught by a free eth_call before any address is published.
    function deployWallet(bytes32 salt) public returns (address wallet) {
        wallet = walletAddress(salt);
        if (wallet.code.length > 0) {
            return wallet;
        }
        bytes memory code = abi.encodePacked(CLONE_PREFIX, implementation, CLONE_SUFFIX);
        address deployed;
        assembly {
            deployed := create2(0, add(code, 0x20), mload(code), salt)
        }
        require(deployed != address(0) && deployed == wallet, "create2 mismatch");
        emit WalletDeployed(salt, wallet);
    }

    function sweep(bytes32 salt, address token) public returns (uint256 moved) {
        address wallet = deployWallet(salt);
        address dest = destination;
        uint256 before = IBalanceOf(token).balanceOf(dest);
        DepositForwarder(payable(wallet)).flushToken(token, dest);
        uint256 afterBalance = IBalanceOf(token).balanceOf(dest);
        require(afterBalance >= before, "destination balance decreased");
        moved = afterBalance - before;
        if (moved > 0) {
            emit Swept(salt, wallet, token, moved);
        }
    }

    function sweepMany(bytes32[] calldata salts, address token) external returns (uint256 total) {
        for (uint256 i = 0; i < salts.length; i++) {
            total += sweep(salts[i], token);
        }
    }

    function sweepNative(bytes32 salt) external returns (uint256 moved) {
        address wallet = deployWallet(salt);
        moved = DepositForwarder(payable(wallet)).flushNative(payable(destination));
        if (moved > 0) {
            emit NativeSwept(salt, wallet, moved);
        }
    }

    function setDestination(address newDestination) external onlyOwner {
        require(newDestination != address(0), "zero destination");
        emit DestinationChanged(destination, newDestination);
        destination = newDestination;
    }

    function transferOwnership(address newOwner) external onlyOwner {
        pendingOwner = newOwner;
        emit OwnershipTransferStarted(owner, newOwner);
    }

    function acceptOwnership() external {
        require(msg.sender == pendingOwner, "not pending owner");
        emit OwnershipTransferred(owner, pendingOwner);
        owner = pendingOwner;
        pendingOwner = address(0);
    }
}
