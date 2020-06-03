# 特性
1. 采用sync.Map为基础进行设计，具有高可用性与安全并发特性。
2. 具有持久化与临时储存功能，持久化采用主备双文件保存，每个数据均使用crc32校验以保证数据的安全、准确性。
3. 支持多种格式直接保存读取，string,bool,int-int64,uint-uint64,nil,float32,float64,[]byte，其他格式使用json反序列化获取

# 使用说明
## Hset(name,value path,expire)

* value支持一下三个格式，会转化为string的key与interface{}的value
  * 任意map
  * struct，这时候会按照struct的成员名字转化key，成员值转为value
  * sync.Map
```go
  cache.Hset("luyu6056",map[string]interface{}{"login_time":1591174524},"member_info") //第四个参数，expire为过期时间，可以省略，默认永久保存
```

## Hget(name,path)
获取取一个cache的结构体用于后续操作，如果不存在则创建一个新的缓存，这里命名为_hset
```go
  _hset := cache.Hget("luyu6056","member_info")  可以利用不同的patch进行批量删除
```

## Has(name,path)
判断缓存是否存在，存在返回缓存对象
```go
  _hset,ok:=cache.Has("luyu6056","member_info")
  if !ok{
    fmt.Println("缓存不存在")
  }
```
## Hdel(name,path)
删除一个缓存
```go
  cache.Hdel("luyu6056","member_info")
```

## Hdel_all(path)
针对path删除一整组缓存
```go
  cache.Hdel("member_info")
```

## _hset.Load(key)
从_hset读取一个缓存数据，返回interface{}和bool
```go
  v,ok := _hset.Load("login_time")
  if ok {
    fmt.Println(v) //int 1591174524
  }
```

## _hset.Get(key,&value)
使用该缓存的[]byte，对value进行反序列化赋值,建议之前保存的格式一致，否则调用Unmarshal会出现其它问题
```go
  var login_time int
  ok := _hset.Get("login_time",&s)
  if ok {
    fmt.Println(login_time) //1591174524
  }
```

## Load_str,Load_int,Load_int64,Load_int32,Load_int16,Load_int8,Load_uint64,Load_float64,Load_float32,Load_bool
尝试对改缓存进行强行转换输出
```go
  s := _hset.Load_str("login_time")
  fmt.Println(s) //1591174524
```
## _hset.Len()
返回hset的长度
```go
  fmt.Println(_hset.Len()) //1
```

## _hset.Range(func(key value)bool)
遍历_hset，key为string，value为interface{}
```go
  _hset.range(func(key string,value interface)bool{
    fmt.Println(key,value) //login_time 1591174524
    return true
  })
```

## _hset.GetExpire()
获取超时时间，返回时间戳
```go
  fmt.Println(_hset.GetExpire()) //-1无限长
```

## _hset.EXPIRE(expire)
在当前时间基础上，设置过期时间，单位秒
```go
  _hset.EXPIRE(30) //30秒后删除
```

## _hset.EXPlREAT(timestamp)
设置过期时间，传入秒级时间戳
```go
  _hset.EXPIRE(time.Now().Unix()+30) //超过当前时间会立即删除
```

## _hset.Hset(value)
写入key-value值，与Hset()一样，支持传入任意map,struct与sync.Map
```go
  _hset.Hset(map[string]interface{}{"login_time":1591174524})
```

## _hset.Set(key,value)
只写入一个key-value
```go
  _hset.Hset("login_time"，1591174524)
```

## _hset.Store(key,value)
写入一个临时key，重启失效
```go
  _hset.Store("temp",true)
```

## _hset.Save()
对所有key进行异步保存，包括上面Store临时新建的key
```go
  _hset.Save()
```

## _hset.Save_r()
对所有key进行立刻保存，程序会阻塞到写入完成
```go
  _hset.Save_r()
```

## _hset.Delete(key)
删掉某个key
```go
  _hset.Delete("temp")
```

