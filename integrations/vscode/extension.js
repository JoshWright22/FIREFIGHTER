// firefighter for VS Code, Cursor and Windsurf: a live status bar item fed by `firefighter events`
// (one JSON object per line), plus commands for status, quotes and cancelling. No dependencies.
const vscode = require('vscode')
const { spawn, execFile } = require('child_process')
const readline = require('readline')

let item, child, output, restartTimer, stopped = false

function bin() {
  return vscode.workspace.getConfiguration('firefighter').get('path') || 'firefighter'
}

function show(line) {
  item.text = line.text.replace('◆', '$(flame)')
  item.tooltip = new vscode.MarkdownString(line.tooltip.split('\n').map(l => l.replace(/[*_`]/g, '')).join('  \n'))
  item.backgroundColor = line.user === 'game' ? new vscode.ThemeColor('statusBarItem.warningBackground') : undefined
  item.show()
}

function listen() {
  if (stopped) return
  child = spawn(bin(), ['events'], { env: { ...process.env, NO_COLOR: '1' }, windowsHide: true })
  readline.createInterface({ input: child.stdout }).on('line', raw => {
    try {
      const ev = JSON.parse(raw)
      if (ev.type === 'status' && ev.line) show(ev.line)
    } catch (_) { /* partial line */ }
  })
  child.on('error', () => {
    item.text = '$(flame) firefighter: not installed'
    item.tooltip = 'Install: see https://github.com/JoshWright22/firefighter'
    item.show()
  })
  child.on('exit', () => {
    if (!stopped) restartTimer = setTimeout(listen, 5000)
  })
}

function run(args) {
  return new Promise(resolve => {
    execFile(bin(), args, { env: { ...process.env, NO_COLOR: '1' }, windowsHide: true, timeout: 20000 }, (err, stdout, stderr) =>
      resolve((stdout || '') + (stderr || '') || String(err || '')))
  })
}

function activate(context) {
  output = vscode.window.createOutputChannel('firefighter')
  item = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 100)
  item.command = 'firefighter.status'
  item.text = '$(flame) firefighter'
  item.show()
  context.subscriptions.push(item, output,
    vscode.commands.registerCommand('firefighter.status', async () => {
      output.clear()
      output.append(await run(['status']))
      output.show(true)
    }),
    vscode.commands.registerCommand('firefighter.quote', async () => {
      const minutes = await vscode.window.showInputBox({ prompt: 'Runtime estimate (minutes)', value: '30' })
      if (!minutes) return
      const vram = await vscode.window.showInputBox({ prompt: 'VRAM GB (blank if unknown)', value: '' })
      const args = ['quote', '--gpu', '--minutes', minutes]
      if (vram) args.push('--vram', vram)
      vscode.window.showInformationMessage((await run(args)).trim(), { modal: false })
    }),
    vscode.commands.registerCommand('firefighter.cancel', async () => {
      const id = await vscode.window.showInputBox({ prompt: 'Job id to cancel' })
      if (id) vscode.window.showInformationMessage((await run(['cancel', id])).trim() || `cancelled ${id}`)
    }))
  listen()
}

function deactivate() {
  stopped = true
  clearTimeout(restartTimer)
  if (child) child.kill()
}

module.exports = { activate, deactivate }
