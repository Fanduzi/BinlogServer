# 先加深，不要先拆 Scheduler

Scheduler 按文件切开（lifecycle / transitions / lease / upload / observability）共享一把锁和同一袋依赖。再拆只会多出透传。决定：先做任务所有权、尽力上传、任务观测（分页和汇总同一次读）、以及元数据查询写明白（禁止类型对不上就当没有）。Scheduler 变窄是这些完成之后的事，不是第一步。
