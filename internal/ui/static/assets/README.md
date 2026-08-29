# internal/ui/static/assets Module

前端构建生成的静态资源子目录。

## Files

| File | Responsibility |
|------|---------------|
| * | 由前端构建工具生成的资源文件；entry hash 由 `make ui-build` 同步并保留 L3 生成产物声明 |

## Exports

- 作为 `/ui/` 资源子路径被浏览器按需加载。
- 当前 hash bundle 与 `internal/ui/static/index.html` 成对发布；starting 或服务端分页 UI 变更后由 `make ui-build` 更新。

## Dependencies

- Upstream: frontend 构建输出。
- Downstream: 浏览器资源请求。

## Update Rule

- 资源组织方式变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
