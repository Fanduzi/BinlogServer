# 尽力上传只留一个调用方

首次上传在执行器、重试在调度器，各有一份 FileUploader，object key 只在一处算、另一处盲信。决定：封文件留在执行器；上传（首次和重试）收成一处。不重开 `docs/develop/plans/2026-03-28-multicloud-storage-adr.md`：各云仍各用官方 SDK，共用 FileUploader。加深的是调用它的那一处，不是再做一层云 SDK。
