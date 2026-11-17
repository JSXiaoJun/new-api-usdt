// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

/// Test-only tokens covering the three transfer conventions the factory must
/// tolerate. Never deployed to a live chain.
contract MockToken {
    mapping(address => uint256) public balanceOf;
    // 0 = returns true, 1 = returns nothing (Ethereum USDT), 2 = returns false (TRON USDT)
    uint8 public immutable mode;

    event Transfer(address indexed from, address indexed to, uint256 value);

    constructor(uint8 mode_) {
        mode = mode_;
    }

    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
        emit Transfer(address(0), to, amount);
    }

    function transfer(address to, uint256 amount) external {
        require(balanceOf[msg.sender] >= amount, "insufficient");
        balanceOf[msg.sender] -= amount;
        balanceOf[to] += amount;
        emit Transfer(msg.sender, to, amount);
        if (mode == 1) {
            return;
        }
        bool result = mode == 0;
        assembly {
            mstore(0, result)
            return(0, 32)
        }
    }
}
