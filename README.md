# sukad
Sukashi node daemon with Xray and native mieru support.
Sukashi 节点服务端，支持 Xray 系协议和原生 mieru。

**注意： 本项目需要搭配 [Sukashi](https://github.com/missuo/sukashi) 面板**

## 软件安装

### 一键安装

```
wget -N https://raw.githubusercontent.com/missuo/sukad/main/script/install.sh && bash install.sh
```

## 构建
``` bash
GOEXPERIMENT=jsonv2 go build -v -o build_assets/sukad -trimpath -ldflags "-X 'github.com/missuo/sukad/cmd.version=$version' -s -w -buildid="
```

## Stars 增长记录

[![Stargazers over time](https://starchart.cc/missuo/sukad.svg?variant=adaptive)](https://starchart.cc/missuo/sukad)
