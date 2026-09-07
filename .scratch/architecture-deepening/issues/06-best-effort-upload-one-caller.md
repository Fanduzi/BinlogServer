# 06: 尽力上传只留一个调用方

**What to build:** 执行器只把文件封好。传到对象存储（第一次和点重试）在一处，用同一把 object key。失败不打断拉流。各家云 SDK 不改。

**Blocked by:** 05

**Status:** done

- [x] 封文件成功、上传失败时，拉流继续，文件记为 UPLOAD_FAILED
- [x] 重试上传用封文件时写下的 object key，传到同一位置
- [x] 只有一份对云的 FileUploader 入口
- [x] 现有上传失败与重试测试改走这一处
