# task045-lognorm

这是一个使用 Go 实现的训练项目源码；具体能力以项目源码和测试为准。

## 本地验证

```bash
go build ./...
go test ./...
go vet ./...
```

## Benzhi Docker 构建

`build_benzhi_docker.sh` 接收可选镜像名和平台参数：

```bash
./build_benzhi_docker.sh my-project linux/amd64
docker run -it my-project:latest
```
