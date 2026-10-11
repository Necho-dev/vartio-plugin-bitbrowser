# vartio-plugin-bitbrowser

Vartio 插件：经探针 TCP 隧道调用远端机器上的比特浏览器本地 API。

插件名是 `bitbrowser`。Hub 从本仓库下载源码，在本机编译成 `vartio-plugin` 后启动。不要把它编进 Hub。

```bash
go test ./...
go build -o vartio-plugin ./cmd/vartio-plugin
```
