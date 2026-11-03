import type { Register } from 'claude-code'

// A command already going through the queue: `firefighter run ...`, optionally after `cd x &&` or
// environment assignments.
const QUEUED = /^\s*(cd\s+("[^"]*"|'[^']*'|\S+)\s*&&\s*)?([A-Za-z_][A-Za-z0-9_]*=\S*\s+)*firefighter(\.exe)?\s+run\b/

// Heavy work an agent should queue: training/eval/benchmark scripts, distributed launchers, and
// GPU containers. Commands that already go through firefighter pass.
const HEAVY =
  /\b(torchrun|deepspeed|accelerate\s+launch|python[0-9.]*\s+(-m\s+)?\S*(train|finetune|fine_tune|pretrain|eval|evaluate|bench|benchmark|sweep|render)\S*|docker\s+run\b[^\n]*--gpus)/i

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function line($: any): Promise<string | undefined> {
  try {
    const { exitCode, stdout } = await $.process.run(['firefighter', 'statusline'], { timeoutMs: 5000, env: { NO_COLOR: '1' } })
    return exitCode === 0 && stdout.trim() !== '' ? stdout.trim() : undefined
  } catch {
    return undefined // firefighter isn't installed: show nothing
  }
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    const result = await next(e)
    await $.command.register({
      name: 'firefighter',
      description: "Who's using the GPU, CPU and RAM, the queue, budgets and the market",
    })
    $.ui.status(await line($))
    $.clock.every(5000, async () => $.ui.status(await line($)))
    return result
  })

  on('command.run', { command: 'firefighter' }, async $ => {
    try {
      const { stdout, stderr } = await $.process.run(['firefighter', 'status'], { timeoutMs: 15000, env: { NO_COLOR: '1' } })
      return { text: stdout.trim() || stderr.trim() }
    } catch {
      return { text: 'firefighter is not installed. Windows: irm https://raw.githubusercontent.com/JoshWright22/FIREFIGHTER/main/scripts/install.ps1 | iex   macOS/Linux: curl -fsSL https://raw.githubusercontent.com/JoshWright22/FIREFIGHTER/main/scripts/install.sh | sh' }
    }
  })

  // Route heavy Bash work through the queue. A guard that fails lets the command through:
  // firefighter's daemon still adopts heavy processes started outside it.
  on('tool.call', { tool: 'Bash' }, ($, e, next) => {
    const cmd = e.command
    if (HEAVY.test(cmd) && !QUEUED.test(cmd)) {
      return {
        deny:
          'firefighter: this looks like heavy work (GPU or long CPU) on a machine shared with other agents and a human who comes first. ' +
          `Run it through the queue as a background task instead: firefighter run --minutes <honest estimate> [--gpu] [--vram GB] -- ${cmd}  ` +
          'It waits for a turn, then runs here and prints a receipt; its replies end with a do: line to follow.',
      }
    }
    return next(e)
  }).catch(($, e, next) => next(e))
}
