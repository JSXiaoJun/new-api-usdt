// Compiles the contracts with solc-js and writes ABI + bytecode for the Go
// tools. evmVersion istanbul avoids PUSH0 and other opcodes TRON may lack.
import fs from 'node:fs'
import path from 'node:path'
import solc from 'solc'

const here = path.dirname(new URL(import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1'))
const sources = {
  'DepositFactory.sol': fs.readFileSync(path.join(here, 'DepositFactory.sol'), 'utf8'),
  'test/MockToken.sol': fs.readFileSync(path.join(here, 'test', 'MockToken.sol'), 'utf8'),
}

const input = {
  language: 'Solidity',
  sources: Object.fromEntries(Object.entries(sources).map(([k, v]) => [k, { content: v }])),
  settings: {
    evmVersion: 'istanbul',
    optimizer: { enabled: true, runs: 200 },
    outputSelection: { '*': { '*': ['abi', 'evm.bytecode.object', 'evm.deployedBytecode.object'] } },
  },
}

const output = JSON.parse(solc.compile(JSON.stringify(input)))
const errors = (output.errors || []).filter((e) => e.severity === 'error')
for (const e of output.errors || []) console.error(e.formattedMessage)
if (errors.length > 0) process.exit(1)

const outDir = path.join(here, 'build')
fs.mkdirSync(outDir, { recursive: true })
const wanted = {
  'DepositFactory.sol': ['DepositFactory', 'DepositForwarder'],
  'test/MockToken.sol': ['MockToken'],
}
for (const [file, names] of Object.entries(wanted)) {
  for (const name of names) {
    const c = output.contracts[file][name]
    const artifact = {
      contractName: name,
      compiler: solc.version(),
      evmVersion: 'istanbul',
      abi: c.abi,
      bytecode: '0x' + c.evm.bytecode.object,
      deployedBytecode: '0x' + c.evm.deployedBytecode.object,
    }
    fs.writeFileSync(path.join(outDir, `${name}.json`), JSON.stringify(artifact, null, 2) + '\n')
    console.log(`wrote build/${name}.json (${c.evm.bytecode.object.length / 2} bytes)`)
  }
}
