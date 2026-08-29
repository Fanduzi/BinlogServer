# internal/ui/static/assets Module

前端构建生成的静态资源子目录。

## Files

| File | Responsibility |
|------|---------------|
| `index-Dkp7m_Rz.js` | 当前前端 entry bundle，包含单请求批量任务创建；由 `make ui-build` 生成并保留 L3 声明 |
| * | 由前端构建工具生成的其他资源文件；历史与 vendor 资源保留 |

## Exports

- 作为 `/ui/` 资源子路径被浏览器按需加载。
- 当前 `index-Dkp7m_Rz.js` 与 `internal/ui/static/index.html` 成对发布；starting、服务端分页或全局/当前页筛选范围 UI 变更后由 `make ui-build` 更新。仅删除被新 index 直接替代的旧 entry。

## Dependencies

- Upstream: frontend 构建输出。
- Downstream: 浏览器资源请求。

## Update Rule

- 资源组织方式变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
