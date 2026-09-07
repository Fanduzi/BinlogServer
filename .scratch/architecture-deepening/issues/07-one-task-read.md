# 07: 任务页和汇总同一次读

**What to build:** 仪表盘/任务列表的分页、总数、汇总、按源计数，用同一套过滤。不要分页走 SQL、汇总再扫全表。

**Blocked by:** 06

**Status:** done

- [x] 同一 host/port/state 过滤下，页 total 与汇总 total 一致
- [x] 有 Store 时不把整表读进内存再切汇总
- [x] 单机仍从内存列表得出同一套数字
- [x] 现有分页与 starting 计数测试仍然通过
