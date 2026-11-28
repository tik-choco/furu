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
| `furu [Claude の引数 ...]` | バージョンとログイン状態を確認し、Git を同期してから Claude を起動します。 |
| `furu --no-update [Claude の引数 ...]` | ログイン状態を確認し、キャッシュ済みのモデル情報を表示し、Git を同期してから Claude を起動します。 |
| `furu --no-pull [Claude の引数 ...]` | Git の fetch、ブランチ切替の確認、fast-forward 更新を省略して Claude を起動します。 |
| `furu doctor` | インストール済みのバージョン、npm の最新版、ログイン状態、使う commit コマンドを表示します。 |
| `furu update` | 古い Claude Code と Codex を更新します。 |
| `furu config` | 設定ファイルのパスと有効な設定を表示し、設定の JSON に続けて使う commit コマンドと選択元を表示します。 |
| `furu model` | モデル設定を表示し、端末からは対話形式で変更できます。 |
| `furu help` | ヘルプを表示します。 |
| `furu mcp` | Claude が起動する stdio MCP サーバーです。 |

個人用に作っているツールです。モデルはファミリー指定で最新版に追従します。

`--resume` や `-c` などの引数は Claude に渡ります。`--model` と `--effort` を指定すると、Orchestrator の既定値を上書きできます。

## 画像の受け渡し

`delegate` の `images` に画像ファイルのパス（絶対パス、または `cwd` からの相対パス）を渡すと、Codex には `-i` で画像を添付します。Claude には `task` にパスを追記し、Read ツールで開くよう指示します。

`write=true` で `cwd` の外にある画像を渡すと、Codex と Claude のどちらも `--add-dir` で画像の親ディレクトリを書き込み可能にします。

Claude Code にパスなしで貼り付けた画像はファイルとして残らないため、worker に渡せません。画像をファイルとして保存してパスを伝えるか、ファイルをドラッグ＆ドロップしてパスを渡してください。

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

`git_sync` は既定で `true` です。起動時に Git の作業ツリーで fetch し、`main` / `master` で `develop` があれば端末で切り替えを確認します。現在のブランチは upstream から fast-forward で更新します。`"git_sync": false` で無効にできます。

### commit コマンド

`commit_command` の既定は自動検知です。未設定、`""`、または `"auto"` のときは PATH 上の `guru` を探します。`guru save --help` の出力に `--no-push` と `--yes` の両方があれば `guru s -y` を使い、それ以外は通常の git を使います。

`"git"` を指定すると自動検知を無効にして通常の git を使います。それ以外のコマンドを明示すると、そのコマンドが自動検知より優先されます。

ラッパーを使う場合、Orchestrator は `<コマンド> "<メッセージ>"` で commit / push を実行します。Claude Code の `permissions.deny` に `Bash(git commit:*)` と `Bash(git push:*)` を追加し、直接実行を禁止します。自動検知した `guru` では、commit のみなら `--no-push`、ステージ済みの変更だけなら `-s` を付けます。commit の author はその PC の git user のままにし、Claude の署名行（Co-Authored-By）は付けません。

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
