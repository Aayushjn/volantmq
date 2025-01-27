package configuration

// defaultConfig loaded anyway when server starts
// may be extended/replaced by user-provided config later
var defaultConfig = []byte(`
version: v0.0.1
system:
  log:
    console:
      level: info # available levels: debug, info, warn, error, dpanic, panic, fatal
  http:
    defaultPort: 8080
  acceptor:
    maxIncoming: 1000
    preSpawn: 100
plugins:
  config:
    auth: # plugin type
      - name: internal
        backend: simpleAuth
        config:
          users:
            testuser: "9f735e0df9a1ddc702bf0a1a7b83033f9f7153a00c29de82cedadc9957289b05" # password must be sha-256 hashed
auth:
  order:
    - internal
mqtt:
  version:
  - v3.1.1
  keepAlive:
    period: 60
    force: false
  options:
    connectTimeout: 2
    offlineQoS0: true
    sessionDups: false
    retainAvail: true
    subsOverlap: false
    subsId: false
    subsShared: false
    subsWildcard: true
    receiveMax: 65535
    maxPacketSize: 268435455
    maxTopicAlias: 65535
    maxQoS: 2
  clientId:
    regex: 
listeners:
  defaultAddr: ""
  mqtt:
    quic:
      1883:
        tls:
          cert: '/home/aayush/volantmq/cmd/volantmq/cert.pem'
          key: '/home/aayush/volantmq/cmd/volantmq/key.pem'
`)
