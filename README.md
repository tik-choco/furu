# furu

`furu` は Claude Code または Codex を Orchestrator として起動する Go 製の CLI です。司令塔は MCP の `delegate` で作業を worker に委譲できます。通常は Claude が司令塔、Codex が worker ですが、Claude が利用できない場合は Codex が司令塔となり、引き続き worker を利用できます。

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
| `furu [Claude の引数 ...]` | バージョンとログイン状態を確認し、Git を同期してから Claude を起動します。利用枠切れを検知した場合は Codex に切り替えます。 |
| `furu codex [Codex の引数 ...]` | Claude の利用枠やログイン状態に関係なく、同じ作業ディレクトリで Codex を司令塔として起動します。 |
| `furu codex --from-claude <session-id または transcript.jsonl>` | 指定した Claude の会話を公式の移行 API で取り込み、Codex の司令塔として再開します。 |
| `furu --no-update [Claude の引数 ...]` | ログイン状態を確認し、キャッシュ済みのモデル情報を表示し、Git を同期してから Claude を起動します。 |
| `furu --no-pull [Claude の引数 ...]` | Git の fetch、ブランチ切替の確認、fast-forward 更新を省略して Claude を起動します。 |
| `furu doctor` | インストール済みのバージョン、npm の最新版、ログイン状態、使う commit コマンドを表示します。 |
| `furu update` | 古い Claude Code と Codex を更新します。 |
| `furu config` | 設定ファイルのパスと有効な設定を表示し、設定の JSON に続けて使う commit コマンドと選択元を表示します。 |
| `furu model` | モデル設定を表示し、端末からは対話形式で変更できます。 |
| `furu status [--cached]` | Claude・Codex の利用枠と委譲実行実績を表示します。`--cached` は CLI に問い合わせません。 |
| `furu help` | ヘルプを表示します。 |
| `furu mcp` | 司令塔が起動する stdio MCP サーバーです。 |

個人用に作っているツールです。モデルはファミリー指定で最新版に追従します。

`--resume` や `-c` などの引数は Claude に渡ります。`--model` と `--effort` を指定すると、Orchestrator の既定値を上書きできます。

### Claude の利用枠が切れた場合

通常起動時に Claude Orchestrator の利用枠が枯渇していることを検知し、Codex が利用できる場合は Codex を司令塔として起動します。Codex にも furu の Orchestrator 用プロンプトと MCP の `delegate` が設定されるため、難易度別の worker、並列委譲、利用枠に応じたルーティングを利用できます。Codex の司令塔と Codex worker は同じ利用枠を共有します。引数なし、単一の初期プロンプト、または明示的な `--resume <Claude のセッション UUID>` が対象です。他の Claude 専用引数は Codex に転送しません。

対話セッション中に利用枠が切れると、Claude の [StopFailure hook](https://code.claude.com/docs/en/hooks#stopfailure) で `rate_limit` を検知して対象の会話を記録します。API エラーが発生しない場合も、未完了のユーザープロンプトがあり利用枠の枯渇を確認できれば移行対象にします。Claude を `/exit` などで正常終了すると、furu が [Codex App Server の公式移行 API](https://learn.chatgpt.com/docs/app-server#detect-and-import-external-agent-config) でその会話だけを移行し、Codex の司令塔として再開します。Claude のプロセスを強制終了せず、終了時に履歴が確定してから移行するため、同じ端末で二つの対話画面が競合しません。Codex には中断した依頼を続ける指示を渡します。

移行済みの同じ履歴は Codex の公式移行記録と内容のハッシュで照合して再利用します。Claude の元の会話や作業ファイルは変更しません。Claude が異常終了した場合は自動移行せず、移行 API が利用できない場合と同様に元のセッション ID を使った再試行方法を表示します。利用枠が回復して Claude で正常にターンを完了した場合は保留中の引き継ぎを取り消します。

手動で会話を引き継ぐ場合は、同じプロジェクトで以下を実行します。

```powershell
furu codex --from-claude <ClaudeのセッションUUID>
```

UUID の代わりに Claude のセッション履歴 `.jsonl` のパスも指定できます。明示的な UUID なしの `-c` などでは別の会話や worker の履歴を誤って取り込まないよう、自動選択を行いません。履歴を特定せずに Codex を起動したい場合は以下を実行してください。

```powershell
furu codex
```

このコマンドは Claude の事前確認と Git の同期を省略します。Codex のモデル設定は Codex の通常設定に従い、furu の MCP 設定は起動時のみ適用されるため、永続的な `config.toml` の変更は不要です。作業中のファイルはそのまま利用できます。Claude の利用枠が回復したら `furu` で再び Claude を起動できます。

セッションの移行処理は [OpenAI 公式の Claude Code 用 Codex プラグイン](https://github.com/openai/codex-plugin-cc/blob/main/plugins/codex/scripts/lib/codex.mjs) と同じ公式 API を使用します。移行の対象は会話であり、実行中のプロセス、承認状態、外部ツールの接続状態などがそのまま移行されるわけではありません。

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

### 利用枠に応じた委譲

既定では Claude の利用枠を計画・レビュー・統合に温存するため、まとまった調査・実装・テストを Codex worker に優先的に委譲します。`delegate` は常時読み込み対象になり、各ユーザープロンプトの開始時に利用状況を Claude に伝えます。直接作業の全面禁止は行いません。

起動時・セッションの稼働中と委譲時には、[Claude Code の公式 `/usage` コマンド](https://code.claude.com/docs/en/commands) と [Codex App Server の `account/rateLimits/read`](https://learn.chatgpt.com/docs/app-server) を使用してサブスクリプションの利用枠を取得します。通常は2分間キャッシュし、10分以上古いデータや取得できない利用枠は不明として扱います。Claude の [status line](https://code.claude.com/docs/en/statusline#rate-limit-usage) からは公式 JSON データも取得し、最新の値がある場合は重複する CLI 問い合わせを省略します。ただしモデル別の利用枠は独立して更新します。API キーからサブスクリプション残量を推定したり、認証トークンを直接読み取ったりしません。

`backend=auto` は Codex 優先を維持し、Codex の使用率が90%以上で Claude が60%以下かつ25ポイント以上の余裕がある場合は Claude worker を利用します。片方の適用対象の利用枠が枯渇している場合は利用可能な方を選び、双方が枯渇している場合はエラーを返します。選択した worker が見つからない、または起動できない場合はもう一方が利用可能なら切り替えます。利用枠が不明な場合は既存の Codex 優先・利用可否判定を使用します。明示的な backend 指定は維持し、既知の枯渇時のみ実行を止めます。worker の起動後に失敗したタスクは別バックエンドに自動再実行しません。

`furu status` で利用枠、リセット時刻、委譲試行・失敗・フォールバックの累計を確認できます。利用率のキャッシュと委譲メタデータ（時刻、モデル、難易度、所要時間、結果、選択理由）は設定ディレクトリ内の `usage/` に保存します。利用率のキャッシュは `CODEX_HOME` / `CLAUDE_CONFIG_DIR` で指定したプロファイルごとに分離します。タスク本文・画像・認証情報は記録しません。

```json
{
  "adaptive_delegation": true,
  "usage_statusline": true
}
```

`adaptive_delegation: false` で動的な利用枠取得・ルーティング・計測・Hook（利用上限時の自動引き継ぎを含む）を無効化できます。`usage_statusline: false` で既存のカスタム status line を維持できます（furu の定期取得とルーティングは継続します）。furu が設定する status line は起動した Claude セッション内のみ適用されます。

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
