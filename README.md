# furu

`furu` は Claude Code を Orchestrator として起動する Go 製の CLI です。Claude は MCP の `delegate` で作業を worker に委譲できます。既定では Codex を使い、Codex が見つからない、ログインしていない、またはモデルのファミリーを解決できない場合は Claude を使います。

## インストール

```powershell
go install github.com/tik-choco/furu/cmd/furu@latest
# またはソースから
go build -o bin/furu.exe ./cmd/furu
```

`furu.exe` と `claude.exe` に PATH を通してください。Codex worker を使う場合は `codex login`、最新版の確認と更新を使う場合は `npm` が必要です。Linux と macOS では `go build -o bin/furu ./cmd/furu` でビルドできます。

## コマンド

| コマンド | 動作 |
| --- | --- |
| `furu [Claude の引数 ...]` | バージョンとログイン状態を確認し、Claude を起動します。 |
| `furu --no-update [Claude の引数 ...]` | ログイン状態を確認し、キャッシュ済みのモデル情報を表示して Claude を起動します。 |
| `furu doctor` | インストール済みのバージョン、npm の最新版、ログイン状態を表示します。 |
| `furu update` | 古い Claude Code と Codex を更新します。 |
| `furu config` | 設定ファイルのパスと有効な設定を JSON で表示します。 |
| `furu model` | モデル設定を表示し、端末からは対話形式で変更できます。 |
| `furu help` | ヘルプを表示します。 |
| `furu mcp` | Claude が起動する stdio MCP サーバーです。 |

個人用に作っているツールです。モデルはファミリー指定で最新版に追従します。

`--resume` や `-c` などの引数は Claude に渡ります。`--model` と `--effort` を指定すると、Orchestrator の既定値を上書きできます。

## 設定

Windows では `%AppData%\furu\config.json` に設定を置けます。実際のパスは `furu config` で確認できます。ファイルがなければ組み込みの既定値を使い、指定した項目だけを上書きします。worker の既定タイムアウトは 30 分です。

```json
{
  "orchestrator": { "effort": "high" },
  "worker_timeout_minutes": 45,
  "tiers": {
    "normal": { "codex": { "effort": "medium" } }
  }
}
```

### commit コマンド

`commit_command` を設定すると、Orchestrator は commit / push にそのコマンドを使い、`git commit` / `git push` を直接使わなくなります（例: 社内・個人のラッパースクリプト）。未設定なら通常の git を使います。どちらの場合も、author はその PC の git user のままにし、Claude の署名行（Co-Authored-By）は付けません。

```json
{
  "commit_command": "my-commit-wrapper -y"
}
```

## モデル設定

既定では Codex の `sol` / `astra` と Claude の `opus` / `sonnet` / `haiku` を使い、各ファミリーの最新モデルに追従します。通常起動時に Codex のカタログを更新し、取得に失敗した場合は前回のキャッシュを使います。固定したい場合はモデル ID を指定します。

```powershell
furu model
furu model list
furu model set hard codex gpt-6.3-sol
furu model set vh codex astra xhigh
furu model set o claude opus max
furu model set n codex --effort high
furu model reset hard
furu model reset
```

`set` で effort を省略すると現在値を維持します。`reset` は指定した対象のモデル設定を既定値に戻し、対象を省略すると全対象を戻します。対象の略称は `o`、`s`、`n`、`h`、`vh`、バックエンドの略称は `cx`（Codex）と `cl`（Claude）です。Orchestrator には Claude を指定できます。

## ライセンス

[MPL-2.0](LICENSE)
