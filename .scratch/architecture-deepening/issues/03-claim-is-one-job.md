# 03: 开机和平时捡任务是同一件事

**What to build:** Worker 开机和运行中的认领走同一条「把该我跑的跑起来」。开机接自己名下的和没人要的 STARTING。平时再捡别人租约已过期的。不要 Stop 别人还活着的 RUNNING。

**Blocked by:** 02

**Status:** done

- [x] 集群开机不会对「别人仍持有未过期租约的 RUNNING」做 Stop+Start
- [x] 没人要的 STARTING 会被捡起并占租约
- [x] 租约已过期的 RUNNING / LEASE_DEGRADED / RETRY_BACKOFF 会被捡起
- [x] 本机已有执行在跑的任务不会再拉起一份
