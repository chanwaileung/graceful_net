## 优雅重启tcp服务

- 生成grpc协议文件
```
protoc test.proto --go_out=plugins=grpc:.
```

- 热更新快捷命令
```
ps -aux|grep main|grep -v grep|awk '{print $2}'|xargs kill -USR1
```
- 结束进程快捷命令
```
ps -aux|grep main|grep -v grep|awk '{print $2}'|xargs kill -9
```