# internal/taskrecords Module

ADR 0010（#385）standalone 任务记录文件 `{data_dir}/.tasks.enc` 的唯一读写方。

## Files
| File | Responsibility |
|------|---------------|
| `taskrecords.go` | 密钥（`ParseKey`、`LoadKey`：`BINLOG_SERVER_TASK_KEY` 或权限不宽于 0600 的普通文件；`GenerateKeyFile`：O_EXCL 0600 新文件）与 `Store`（`Open`、`Load`、`Save`、`RemoveStaleTemps`）。文件是带版本的 JSON 信封 `{format, version, key_id, nonce, ciphertext}`，AES-256-GCM，信封头进 AAD；key id 为 SHA-256("binlog-server task records v1"‖key) 前 8 字节。写入：O_EXCL 0600 临时文件、fsync、rename、目录 fsync。`Load` 区分 `ErrNoFile`、`ErrKeyMissing`、`ErrWrongKey`、`ErrCorrupt`（含路径），用上一把密钥能解开时告诉调用方以便用当前密钥重存。 |
| `taskrecords_test.go` | 往返、错密钥、损坏/未知版本、篡改头被 AAD 拒绝、上一把密钥、原子写残留清理、文件和密钥文件权限。 |

## Dependencies
- Upstream: `internal/tasks`（`RecordFile`）、`internal/app`、`cmd/binlog-server/cmd`（`task-key generate`）
- Downstream: `crypto/aes`、`crypto/cipher`、`crypto/sha256`、`crypto/rand`

## Update Rule
- 文件格式、版本、密钥来源或错误分类变化时，更新本文件和 ADR 0010。
