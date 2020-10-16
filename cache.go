package cache

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"math"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
	"unsafe"

	jsoniter "github.com/json-iterator/go"
	"github.com/klauspost/compress/gzip"
	//"unsafe"
	//"strings"
)

var cachebufpool = sync.Pool{
	New: func() interface{} {
		return &MsgBuffer{}
	},
}

//本cache包会监听ctrl+c事件以保证缓存被正确保存
const (
	CACHE_FILE_NAME = "db.cache" //持久化储存的文件名
	CACHE_Path      = "./cache"
	ISDEBUG         = true       //是否fmt打印错误
	SAVE_TIME       = 1          //持久化间隔时间,单位秒
	GZIP_LIMIT      = 4096       //大于这个尺寸就压缩
	MAXLEN          = 1073741824 //128M 缓存单条消息大于这个尺寸就抛弃
	GZIP_LEVEL      = 6          //压缩等级
)

var (
	hashcache        sync.Map                                                                                   //储存变量
	hashcache_q      []*writeHash                                                                               //写入队列 5000定长
	hashcache_q_m    sync.Map                                                                                   //写入队列的map
	hashdelete       sync.Map                                                                                   //待删除变量 map[int64][]map[string]string
	queueLock        sync.Mutex                                                                                 //写入队列锁
	writehashLock    sync.RWMutex                                                                               //writehash锁
	unserialize_func []func(bin []byte) (*hashvalue, error) = make([]func(bin []byte) (*hashvalue, error), 256) //反序列化方法
	write_lock       sync.Mutex                                                                                 //写文件锁
	hash_sync_chan   chan []*writeHash                      = make(chan []*writeHash, 1)
)

//gzip相关
var uncompress_chan = make(chan *bytes.Buffer, runtime.NumCPU())
var gzipcompress_chan = make(chan *Gzip_writer, runtime.NumCPU())

type Gzip_writer struct {
	Buf    *MsgBuffer
	Writer *gzip.Writer
}

type Hashvalue struct { //缓存结构
	value      *sync.Map
	writevalue *writeHash //避免多个副本写入多次数据
	update     bool       //更新标志
	path_m     *sync.Map
}
type writeHash struct {
	path   string //本条缓存所在的path
	name   string //本条缓存所在的name
	un_set bool
	value  *sync.Map
	expire int64 //正值为过期删除时间,其他含义如下
}

const (
	expire_keep        = -4 //不处理
	expire_delete_path = -3 //删除path
	expire_delete_name = -2 //删除
	expire_ever        = -1 //永久缓存
	expire_none        = 0
)

type hashvalue struct {
	//b   []byte //原始值
	i   interface{}
	typ string //缓存的类型
	str string //普通字串解析
	i64 uint64
}

func new_hashvalue(value interface{}) *hashvalue {
	h := new(hashvalue)
	h.i = value
	h.typ = reflect.TypeOf(value).String()
	switch value.(type) {
	case string:
		h.str = value.(string)
		i, _ := strconv.Atoi(value.(string))
		h.i64 = uint64(i)
	case int:
		h.i64 = uint64(value.(int))
		h.str = strconv.Itoa(int(value.(int)))
	case int8:
		h.i64 = uint64(value.(int8))
		h.str = strconv.Itoa(int(value.(int8)))
	case int16:
		h.i64 = uint64(value.(int16))
		h.str = strconv.Itoa(int(value.(int16)))
	case int32:
		h.i64 = uint64(value.(int32))
		h.str = strconv.Itoa(int(value.(int32)))
	case int64:
		h.i64 = uint64(value.(int64))
		h.str = strconv.Itoa(int(value.(int64)))
	case uint:
		h.i64 = uint64(value.(uint))
		h.str = strconv.FormatUint(uint64(value.(uint)), 10)
	case uint8:
		h.i64 = uint64(value.(uint8))
		h.str = strconv.FormatUint(uint64(value.(uint8)), 10)
	case uint16:
		h.i64 = uint64(value.(uint16))
		h.str = strconv.FormatUint(uint64(value.(uint16)), 10)
	case uint32:
		h.i64 = uint64(value.(uint32))
		h.str = strconv.FormatUint(uint64(value.(uint32)), 10)
	case uint64:
		h.i64 = value.(uint64)
		h.str = strconv.FormatUint(uint64(value.(uint64)), 10)
	case bool:
		h.i64 = 0
		h.str = "false"
		if value.(bool) {
			h.i64 = 1
			h.str = "true"
		}
	case *hashvalue:
		return value.(*hashvalue)
	case float32, float64:
		h.str = fmt.Sprint(value)
	default:

	}
	//h.b = jsoniter.Marshal(i)
	return h
}

//path层
/*type cache_path struct {
	cache *sync.Map
	//hot_step    int64
	//hot_max     int64
	//hot_num_max int64
}*/
func (this *Hashvalue) Load(key string) (interface{}, bool) {

	result, ok := this.value.Load(key)
	if !ok {
		return nil, false
	}
	return result.(*hashvalue).i, true

}

func (this *Hashvalue) Get(key string, value interface{}) bool {

	result, ok := this.value.Load(key)
	if !ok {
		return false
	}
	res := result.(*hashvalue)
	r := reflect.TypeOf(value)

	if r.Kind() != reflect.Ptr {
		return false
	}
	//DEBUG(key, r.String(), res.tpy, res.b)
	r = r.Elem()
	if r.String() == res.typ {
		reflect.ValueOf(value).Elem().Set(reflect.ValueOf(res.i))
	} else {
		if b, ok := res.i.([]byte); ok {
			err := jsoniter.Unmarshal(b, value)
			if err == nil {
				res.i = reflect.ValueOf(value).Elem().Interface()
				res.typ = r.String()
			} else {
				DEBUG(err, this.writevalue.name, this.writevalue.path)
			}
		}

	}

	return true
}

//Load返回string,以下load扩展方法不支持传入struct
func (this *Hashvalue) Load_str(key string) string {
	result, ok := this.value.Load(key)
	if !ok {
		return ""
	}
	return result.(*hashvalue).str
}

//load返回int
func (this *Hashvalue) Load_int(key string) int {
	result, ok := this.value.Load(key)
	if !ok {
		return 0
	}
	return int(result.(*hashvalue).i64)
}

//load返回int64
func (this *Hashvalue) Load_int64(key string) int64 {
	result, ok := this.value.Load(key)
	if !ok {
		return 0
	}
	return int64(result.(*hashvalue).i64)
}

//load返回int32
func (this *Hashvalue) Load_int32(key string) int32 {
	result, ok := this.value.Load(key)
	if !ok {
		return 0
	}
	return int32(result.(*hashvalue).i64)
}

//load返回int16
func (this *Hashvalue) Load_int16(key string) int16 {
	result, ok := this.value.Load(key)
	if !ok {
		return 0
	}
	return int16(result.(*hashvalue).i64)
}

//load返回int8
func (this *Hashvalue) Load_int8(key string) int8 {
	result, ok := this.value.Load(key)
	if !ok {
		return 0
	}
	return int8(result.(*hashvalue).i64)
}

//load返回uint64
func (this *Hashvalue) Load_uint64(key string) uint64 {
	result, ok := this.value.Load(key)
	if !ok {
		return 0
	}
	return result.(*hashvalue).i64
}

//load返回float64
func (this *Hashvalue) Load_float64(key string) float64 {
	result, ok := this.value.Load(key)
	if !ok {
		return 0
	}

	f, _ := strconv.ParseFloat(result.(*hashvalue).str, 64)
	return f
}

//load返回float32
func (this *Hashvalue) Load_float32(key string) float32 {
	result, ok := this.value.Load(key)
	if !ok {
		return 0
	}
	f, _ := strconv.ParseFloat(result.(*hashvalue).str, 32)
	return float32(f)
}
func (this *Hashvalue) Load_bool(key interface{}) bool {
	result, ok := this.value.Load(key)
	if !ok {
		return false
	}
	return result.(*hashvalue).i64 == 1
}
func (this *Hashvalue) Len() (length int) {
	this.value.Range(func(k, v interface{}) bool {
		length += 1
		return true
	})
	return
}
func (this *Hashvalue) Range(f func(string, interface{}) bool) {
	this.value.Range(func(k, v interface{}) bool {
		return f(k.(string), v.(*hashvalue).i)
	})
}

func (this *Hashvalue) GetExpire() int64 {
	if this.writevalue.expire > time.Now().Unix() {
		return this.writevalue.expire
	}
	return -1
}

/**
 *使用Hset,Hset_r可以保存本地文件持久化
 *使用Store方法，可以临时保存内容到缓存，重启失效
 **/
func (this *Hashvalue) Store(key string, value interface{}) {

	result, ok := this.value.Load(key)

	if ok {

		switch value.(type) {
		case string:
			result.(*hashvalue).str = value.(string)
			i, _ := strconv.Atoi(value.(string))
			result.(*hashvalue).i64 = uint64(i)
		case int:
			result.(*hashvalue).i64 = uint64(value.(int))
			result.(*hashvalue).str = strconv.Itoa(int(value.(int)))
		case int8:
			result.(*hashvalue).i64 = uint64(value.(int8))
			result.(*hashvalue).str = strconv.Itoa(int(value.(int8)))
		case int16:
			result.(*hashvalue).i64 = uint64(value.(int16))
			result.(*hashvalue).str = strconv.Itoa(int(value.(int16)))
		case int32:
			result.(*hashvalue).i64 = uint64(value.(int32))
			result.(*hashvalue).str = strconv.Itoa(int(value.(int32)))
		case int64:
			result.(*hashvalue).i64 = uint64(value.(int64))
			result.(*hashvalue).str = strconv.Itoa(int(value.(int64)))
		case uint:
			result.(*hashvalue).i64 = uint64(value.(uint))
			result.(*hashvalue).str = strconv.FormatUint(uint64(value.(uint)), 10)
		case uint8:
			result.(*hashvalue).i64 = uint64(value.(uint8))
			result.(*hashvalue).str = strconv.FormatUint(uint64(value.(uint8)), 10)
		case uint16:
			result.(*hashvalue).i64 = uint64(value.(uint16))
			result.(*hashvalue).str = strconv.FormatUint(uint64(value.(uint16)), 10)
		case uint32:
			result.(*hashvalue).i64 = uint64(value.(uint32))
			result.(*hashvalue).str = strconv.FormatUint(uint64(value.(uint32)), 10)
		case uint64:
			result.(*hashvalue).i64 = value.(uint64)
			result.(*hashvalue).str = strconv.FormatUint(uint64(value.(uint64)), 10)
		case bool:
			result.(*hashvalue).i64 = 0
			result.(*hashvalue).str = "false"
			if value.(bool) {
				result.(*hashvalue).i64 = 1
				result.(*hashvalue).str = "true"
			}
		case *hashvalue:
			this.value.Store(key, value)
			this.update = true
			return
		case float32, float64:
			result.(*hashvalue).str = fmt.Sprint(value)
		}
		result.(*hashvalue).i = value
	} else {
		write := new(sync.Map)
		write.Store(key, new_hashvalue(value))
		this.do_hash(write, expire_keep, "")
		this = Hget(this.writevalue.name, this.writevalue.path)
	}
	this.update = true
}

func (this *Hashvalue) Delete(key string) {
	this.update = true
	result, ok := this.value.Load(key)
	if ok {
		result.(*hashvalue).i64 = 0
		result.(*hashvalue).str = ""
		result.(*hashvalue).i = nil
		result.(*hashvalue).typ = ""
		this.writevalue.value.Store(key, result)
		this.value.Delete(key)
		hash_write(map[string]map[string]*writeHash{this.writevalue.path: map[string]*writeHash{this.writevalue.name: this.writevalue}})
	}

}

//删除掉所有数据
func (this *Hashvalue) Hdel() {
	Hdel(this.writevalue.name, this.writevalue.path)
}
func (this *Hashvalue) Hset(value interface{}) bool {
	this.update = false
	return this.do_hash(value, expire_keep, "hset")
}

func (this *Hashvalue) Set(key string, value interface{}) bool {
	val := new_hashvalue(value)
	this.update = false
	this.writevalue.value.Store(key, val)
	return this.do_hash(this.writevalue.value, expire_keep, "hset")
}

//保存整条缓存
func (this *Hashvalue) Save() {
	if this.update == false {
		return
	}
	this.update = false
	//加入写队列
	this.value.Range(func(k, v interface{}) bool {
		this.writevalue.value.Store(k, v)
		return true
	})
	hash_queue(this.writevalue)
	//return this.do_hash(this.key, this.value, this.path, expire[0], "hset")
}

//保存整条缓存
func (this *Hashvalue) Save_r() {
	if this.update == false {
		return
	}
	this.value.Range(func(k, v interface{}) bool {
		this.writevalue.value.Store(k, v)
		return true
	})

	hash_write(map[string]map[string]*writeHash{this.writevalue.path: map[string]*writeHash{this.writevalue.name: this.writevalue}})
}

//设置超时
func (this *Hashvalue) Expire(expire int64) {
	if expire <= 0 {
		this.Hdel()
	} else {
		this.update = true
		this.do_hash(nil, expire, "expire")
	}
}

//设置超时删除，到期删除这一组数据，参数时间戳
func (this *Hashvalue) ExpireAt(timestamp int64) {
	expire := timestamp - time.Now().Unix()
	if expire <= 0 {
		this.Hdel()
	} else {
		this.update = true
		this.do_hash(nil, expire, "expire")
	}
}

/**
 * 持久化写入value支持以下类型
 * map,可以传入map[数字]、map[string],注意后半部分支持类型,建议使用map[string]interface{}
 * sync.Map
 * struct,多重struct套嵌可能会无法持久化
 **/
func (this *Hashvalue) do_hash(value_i interface{}, expire int64, t string) bool {

	//if t == "hdel" {
	//DEBUG(key, path, "删除")
	//panic("存在删除")
	//}
	if value_i == nil && t != "expire" {
		return false
	}

	value, ok := get_value(value_i)
	if !ok {
		return false
	}
	writehashLock.RLock()
	defer writehashLock.RUnlock()
	path_v := this.path_m
	var writevalue_new bool
	//对原始缓存进行更新
	if value != nil {
		value.Range(func(k, v interface{}) bool {
			if w_v, ok := this.writevalue.value.LoadOrStore(k, v); ok {
				if w_v.(*hashvalue).typ != v.(*hashvalue).typ || w_v.(*hashvalue).str != v.(*hashvalue).str {
					this.writevalue.value.Store(k, v)
					writevalue_new = true
				}
			}
			if v_v, ok := this.value.LoadOrStore(k, v); ok {
				if v_v.(*hashvalue).typ != v.(*hashvalue).typ || v_v.(*hashvalue).str != v.(*hashvalue).str {
					this.value.Store(k, v)
				}
			}

			return true
		})
	}

	//持久化时间 -4不设置超时信息，继承旧值或者初始化
	if expire == expire_keep {
		if this.writevalue.expire != 0 {
			//继承旧值
			expire = this.writevalue.expire
		} else {
			//初始化永久缓存
			expire = expire_ever
		}
	} else {
		if expire > 0 {
			expire = time.Now().Unix() + expire
			queueLock.Lock()
			if v, ok := hashdelete.Load(expire); ok {
				h := v.([]map[string]string)
				h = append(h, map[string]string{"name": this.writevalue.name, "path": this.writevalue.path})
			} else {
				hashdelete.Store(expire, []map[string]string{map[string]string{"name": this.writevalue.name, "path": this.writevalue.path}})
			}
			queueLock.Unlock()
		} else {
			expire = expire_ever
		}
	}
	if !writevalue_new && this.writevalue.expire == expire {
		return true //没有任何变化，不触发写入
	}
	//赋值，写入持久化
	this.writevalue.expire = expire

	switch t {
	case "":
		path_v.Store(this.writevalue.name, this)
		hashcache.Store(this.writevalue.path, path_v)
	case "hset":
		this.update = false
		//加入写队列
		hash_queue(this.writevalue)
	}
	return true
}

//结构体转sync.Map用于持久化写入
func get_value(value interface{}) (write *sync.Map, ok bool) {

	switch value.(type) {
	case sync.Map:
		a := value.(sync.Map)
		return &a, true
	case *sync.Map:
		return value.(*sync.Map), true
	case string:
		return
	case uint64:
		return
	case uint32:
		return
	case uint16:
		return
	case uint8:
		return
	case uint:
		return
	case int64:
		return
	case int32:
		return
	case int16:
		return
	case int8:
		return
	case int:
		return
	case float32:
		return
	case float64:
		return
	case bool:
		return
	case nil:

	default:
		write = new(sync.Map)
		object := reflect.ValueOf(value)
		k := object.Kind()
		if k == reflect.Ptr {
			object = object.Elem() //指针转换为对应的结构
			k = object.Kind()
		}
		switch k {
		case reflect.Struct:
			myref := object
			typeOfType := myref.Type()
			for i := 0; i < myref.NumField(); i++ {
				if typeOfType.Field(i).Name != "" {
					write.Store(typeOfType.Field(i).Name, new_hashvalue(value))
					return
				}

			}
		case reflect.Map:
			for _, key := range object.MapKeys() {
				write.Store(fmt.Sprint(key.Interface()), new_hashvalue(object.MapIndex(key).Interface()))
			}
		default:
			DEBUG("反射类型未设置", k)
		}

	}
	ok = true
	return
}

/**
 * 持久化写出增量文件
 */
type Hash_file struct {
	P string
	K string
	V map[string][]byte
	T int64
}

func hash_write(write map[string]map[string]*writeHash) {
	write_lock.Lock()
	defer write_lock.Unlock()

	f1, err1 := os.OpenFile(CACHE_Path+"/"+CACHE_FILE_NAME, os.O_APPEND|os.O_RDWR|os.O_CREATE, 0666)
	if err1 != nil {
		DEBUG(err1, "hash文件创建失败")
		return
	}
	defer f1.Close()
	f2, err2 := os.OpenFile(CACHE_Path+"/"+CACHE_FILE_NAME+".bak", os.O_APPEND|os.O_RDWR|os.O_CREATE, 0666)
	if err2 != nil {
		DEBUG(err2, "hash文件创建失败")
		return
	}
	defer f2.Close()
	write_file_func(write, f1)
	write_file_func(write, f2)
	for _, v := range write {
		for _, val := range v {
			val.value.Range(func(key, _ interface{}) bool {
				val.value.Delete(key) //写完了删掉
				return true
			})
		}
	}
}

func write_file_func(write map[string]map[string]*writeHash, f *os.File) {
	//var writeString Hash_file
	b1 := cachebufpool.Get().(*MsgBuffer)
	b2 := cachebufpool.Get().(*MsgBuffer)
	write_buf := cachebufpool.Get().(*MsgBuffer)
	defer func() {
		if err := recover(); err != nil {
			fmt.Println(err)
			debug.PrintStack()
		}
		cachebufpool.Put(b1)
		cachebufpool.Put(b2)
		cachebufpool.Put(write_buf)
	}()
	write_buf.Reset()
	gzip_b := new(bytes.Buffer)
	gzip_b.Grow(1024 * 1024)
	gzip, _ := gzip.NewWriterLevel(gzip_b, GZIP_LEVEL)

	for path, v := range write {
		for key, val := range v {

			b1.Reset()
			b2.Reset()
			val.value.Range(func(k1, v1 interface{}) bool {
				switch v1.(type) {
				case *hashvalue:
				default:
					DEBUG(k1)
					Log("%+v", k1)
				}
				write_string, ok := serialize(v1.(*hashvalue))
				if ok {
					b := []byte(k1.(string))
					binary.Write(b2, binary.LittleEndian, uint16(len(b)))
					b2.Write(b)
					binary.Write(b2, binary.LittleEndian, uint32(len(write_string)))
					b2.Write(write_string)
				} else {
					DEBUG(path, key, "写入失败")
				}
				return true
			})
			b := []byte(val.path)
			binary.Write(b1, binary.LittleEndian, uint16(len(b)))
			b1.Write(b)
			b = []byte(val.name)

			binary.Write(b1, binary.LittleEndian, uint16(len(b)))
			b1.Write(b)
			binary.Write(b1, binary.LittleEndian, uint64(val.expire))
			binary.Write(b1, binary.LittleEndian, uint32(b2.Len()))
			b1.Write(b2.Bytes())
			if val.un_set == true {
				patch, _ := hashcache.Load(val.path)
				patch.(*sync.Map).Delete(val.name)
			}
			is_compress := false
			if b1.Len() > GZIP_LIMIT {
				gzip_b.Reset()
				gzip.Reset(gzip_b)
				gzip.Write(b1.Bytes())
				if err := gzip.Flush(); err == nil {
					if err = gzip.Close(); err == nil {
						is_compress = true
					}
				}
			}
			if is_compress {
				write_b := make([]byte, gzip_b.Len())
				copy(write_b, gzip_b.Bytes())
				tmp := write_buf.Make(4)
				binary.LittleEndian.PutUint32(tmp, uint32(len(write_b)+5))
				write_buf.WriteByte(1)
				write_buf.Write(Crc32_check(write_b))
				write_buf.Write(write_b)
			} else {

				write_b := make([]byte, b1.Len())
				copy(write_b, b1.Bytes())
				tmp := write_buf.Make(4)

				binary.LittleEndian.PutUint32(tmp, uint32(len(write_b)+5))
				write_buf.WriteByte(0)
				write_buf.Write(Crc32_check(write_b))
				write_buf.Write(write_b)
			}
		}
	}
	n, err := f.Write(write_buf.Bytes())
	if n != write_buf.Len() {
		DEBUG("写出长度不对")
	}
	if err != nil {
		DEBUG(err)
	}
}

/**
 *持久化写出db文件
 **/
func hash_write_db() {
	write_lock.Lock()
	defer write_lock.Unlock()
	//结构体无法序列化，需要转换
	f1, err1 := os.Create(CACHE_Path + "/" + CACHE_FILE_NAME)
	defer f1.Close()
	if err1 != nil {
		DEBUG(err1, "hash_db文件创建失败")
		return
	}

	f2, err2 := os.Create(CACHE_Path + "/" + CACHE_FILE_NAME + ".bak")
	defer f2.Close()
	if err2 != nil {
		DEBUG(err1, "hash_db备份文件创建失败")
		return
	}

	write := map[string]map[string]*writeHash{}
	hashcache.Range(func(path_i, val_i interface{}) bool {
		val_i.(*sync.Map).Range(func(key_i, v_i interface{}) bool {
			write_hash := new(writeHash)
			write_hash.value = new(sync.Map)
			write_hash.path = v_i.(*Hashvalue).writevalue.path
			write_hash.name = v_i.(*Hashvalue).writevalue.name
			write_hash.expire = v_i.(*Hashvalue).writevalue.expire
			v_i.(*Hashvalue).value.Range(func(kk_i, vv interface{}) bool {
				write_hash.value.Store(kk_i, vv)
				return true
			})
			if write[v_i.(*Hashvalue).writevalue.path] == nil {
				write[v_i.(*Hashvalue).writevalue.path] = make(map[string]*writeHash)
			}
			write[v_i.(*Hashvalue).writevalue.path][v_i.(*Hashvalue).writevalue.name] = write_hash
			return true
		})
		return true
	})
	write_file_func(write, f1)
	write_file_func(write, f2)

}

//仅用于校验数据完整性
func Crc32_check(s []byte) []byte {
	bin := make([]byte, 4)
	binary.LittleEndian.PutUint32(bin, crc32.ChecksumIEEE(s))
	return bin
}

/**
 *使用序列化进行持久化,仅支持以下类型持久化，如有其它需求可以自行添加序列化和反序列化方法
 *
 **/
const (
	serialize_string = iota
	serialize_bool
	serialize_int
	serialize_int8
	serialize_int16
	serialize_int32
	serialize_int64
	serialize_uint
	serialize_uint8
	serialize_uint16
	serialize_uint32
	serialize_uint64
	serialize_mss
	serialize_msi
	serialize_msI
	serialize_Ss
	serialize_Smss
	serialize_SmsI
	serialize_msmss
	serialize_mi32mss
	serialize_msmsI
	serialize_mis
	serialize_mimsI
	serialize_nil
	serialize_f32
	serialize_f64
	serialize_byte
	serialize_default
	serialize_delete
)

func serialize(vv *hashvalue) ([]byte, bool) {
	ok := true
	buf := cachebufpool.Get().(*MsgBuffer)
	b := cachebufpool.Get().(*MsgBuffer)
	defer func() {
		cachebufpool.Put(buf)
		cachebufpool.Put(b)
	}()
	b.Reset()
	buf.Reset()
	switch vv.typ {
	case "string":
		buf.WriteByte(serialize_string)
		buf.Write(Crc32_check([]byte(vv.str)))
		buf.WriteString(vv.str)
	case "bool":
		data := "1"
		if vv.i64 == 0 {
			data = "0"
		}
		buf.WriteByte(serialize_bool)
		buf.Write(Crc32_check([]byte(data)))
		buf.WriteString(data)
	case "int":
		tmp := b.Make(8)
		binary.LittleEndian.PutUint64(tmp, vv.i64)
		buf.WriteByte(serialize_int)
		buf.Write(Crc32_check(tmp))
		buf.Write(tmp)
	case "int8":
		buf.WriteByte(serialize_int8)
		buf.Write(Crc32_check([]byte{uint8(vv.i64)}))
		buf.WriteByte(uint8(vv.i64))
	case "int16":
		tmp := b.Make(2)
		binary.LittleEndian.PutUint16(tmp, uint16(vv.i64))
		buf.WriteByte(serialize_int16)
		buf.Write(Crc32_check(tmp))
		buf.Write(tmp)
	case "int32":
		tmp := b.Make(4)
		binary.LittleEndian.PutUint32(tmp, uint32(vv.i64))
		buf.WriteByte(serialize_int32)
		buf.Write(Crc32_check(tmp))
		buf.Write(tmp)
	case "int64":
		tmp := b.Make(8)
		binary.LittleEndian.PutUint64(tmp, vv.i64)
		buf.WriteByte(serialize_int64)
		buf.Write(Crc32_check(tmp))
		buf.Write(tmp)
	case "uint":
		tmp := b.Make(8)
		binary.LittleEndian.PutUint64(tmp, vv.i64)
		buf.WriteByte(serialize_uint)
		buf.Write(Crc32_check(tmp))
		buf.Write(tmp)
	case "uint8":
		buf.WriteByte(serialize_uint8)
		buf.Write(Crc32_check([]byte{uint8(vv.i64)}))
		buf.WriteByte(uint8(vv.i64))
	case "uint16":
		tmp := b.Make(2)
		binary.LittleEndian.PutUint16(tmp, uint16(vv.i64))
		buf.WriteByte(serialize_uint16)
		buf.Write(Crc32_check(tmp))
		buf.Write(tmp)
	case "uint32":
		tmp := b.Make(4)
		binary.LittleEndian.PutUint32(tmp, uint32(vv.i64))
		buf.WriteByte(serialize_uint32)
		buf.Write(Crc32_check(tmp))
		buf.Write(tmp)
	case "uint64":
		tmp := b.Make(8)
		binary.LittleEndian.PutUint64(tmp, vv.i64)
		buf.WriteByte(serialize_uint64)
		buf.Write(Crc32_check(tmp))
		buf.Write(tmp)
	case "nil":
		buf.WriteByte(serialize_nil)
		buf.Write(Crc32_check(nil))
	case "float32":
		tmp := b.Make(4)
		f, _ := strconv.ParseFloat(vv.str, 32)
		binary.LittleEndian.PutUint32(tmp, math.Float32bits(float32(f)))
		buf.WriteByte(serialize_f32)
		buf.Write(Crc32_check(tmp))
		buf.Write(tmp)
	case "float64":
		tmp := b.Make(8)
		f, _ := strconv.ParseFloat(vv.str, 64)
		binary.LittleEndian.PutUint64(tmp, math.Float64bits(f))
		buf.WriteByte(serialize_f64)
		buf.Write(Crc32_check(tmp))
		buf.Write(tmp)
	case "[]byte":
		buf.WriteByte(serialize_byte)
		buf.Write(Crc32_check(vv.i.([]byte)))
		buf.Write(vv.i.([]byte))
	case "":
		buf.WriteByte(serialize_delete)
		buf.Write(Crc32_check(nil))
		buf.Write(nil)
	default:
		data, _ := jsoniter.Marshal(vv.i)
		buf.WriteByte(serialize_default)
		buf.Write(Crc32_check(data))
		buf.Write(data)
		out := make([]byte, buf.Len())
		copy(out, buf.Bytes())
		return out, true

	}
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, ok
}

//对应的反序列化方法
func unserialize(vv []byte) (*hashvalue, error) {
	read_type := vv[0]
	if Bytes2str(Crc32_check(vv[5:])) != Bytes2str(vv[1:5]) {
		DEBUG("警告：反序列化失败，数据完整性验证失败")
		err := errors.New("警告：反序列化失败，数据完整性验证失败")
		return nil, err
	}
	b := make([]byte, len(vv)-5)
	copy(b, vv[5:]) //避免数据错乱
	if f := unserialize_func[read_type]; f != nil {
		return f(b)
	} else if read_type != serialize_nil {
		panic("cache反序列化失败，类型：" + string(read_type) + " 原始值：" + Bytes2str(vv[5:]))
	}
	return nil, nil
}

//初始化反序列化方法
func init_unserialize_func() {
	unserialize_func[serialize_string] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "string"
		val.str = string(bin)
		val.i = val.str
		i, _ := strconv.Atoi(val.str)
		val.i64 = uint64(i)
		return val, nil
	}
	unserialize_func[serialize_bool] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "bool"
		val.str = "false"
		val.i = false
		if bin[0] == 49 {
			val.i64 = 1
			val.str = "true"
			val.i = true
		}

		return val, nil
	}
	unserialize_func[serialize_int] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "int"
		val.i64 = binary.LittleEndian.Uint64(bin)
		val.str = strconv.Itoa(int(val.i64))
		val.i = int(val.i64)
		return val, nil
	}
	unserialize_func[serialize_int8] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "int8"
		val.i64 = uint64(bin[0])
		val.str = strconv.Itoa(int(val.i64))
		val.i = int8(val.i64)
		return val, nil
	}
	unserialize_func[serialize_int16] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "int16"
		val.i64 = uint64(binary.LittleEndian.Uint16(bin))
		val.str = strconv.Itoa(int(val.i64))
		val.i = int16(val.i64)
		return val, nil
	}
	unserialize_func[serialize_int32] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "int32"
		val.i64 = uint64(binary.LittleEndian.Uint32(bin))
		val.str = strconv.Itoa(int(val.i64))
		val.i = int32(val.i64)
		return val, nil

	}
	unserialize_func[serialize_int64] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "int64"
		val.i64 = binary.LittleEndian.Uint64(bin)
		val.str = strconv.Itoa(int(val.i64))
		val.i = int64(val.i64)
		return val, nil
	}
	unserialize_func[serialize_uint] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "uint"
		val.i64 = binary.LittleEndian.Uint64(bin)
		val.str = strconv.FormatUint(val.i64, 10)
		val.i = uint(val.i64)
		return val, nil
	}
	unserialize_func[serialize_uint8] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "uint8"
		val.i64 = uint64(bin[0])
		val.str = strconv.FormatUint(val.i64, 10)
		val.i = uint8(val.i64)
		return val, nil
	}
	unserialize_func[serialize_uint16] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "uint16"
		val.i64 = uint64(binary.LittleEndian.Uint16(bin))
		val.str = strconv.FormatUint(val.i64, 10)
		val.i = uint16(val.i64)
		return val, nil
	}
	unserialize_func[serialize_uint32] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "uint32"
		val.i64 = uint64(binary.LittleEndian.Uint32(bin))
		val.str = strconv.FormatUint(val.i64, 10)
		val.i = uint32(val.i64)
		return val, nil
	}
	unserialize_func[serialize_uint64] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "uint64"
		val.i64 = binary.LittleEndian.Uint64(bin)
		val.str = strconv.FormatUint(val.i64, 10)
		val.i = uint64(val.i64)
		return val, nil

	}
	unserialize_func[serialize_f32] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "float32"
		f := math.Float32frombits(binary.LittleEndian.Uint32(bin))
		val.str = fmt.Sprint(f)
		val.i = f
		return val, nil
	}
	unserialize_func[serialize_f64] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "float64"
		f := math.Float64frombits(binary.LittleEndian.Uint64(bin))
		val.str = fmt.Sprint(f)
		val.i = f
		return val, nil
	}
	unserialize_func[serialize_byte] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{i: bin}
		val.typ = "[]byte"
		return val, nil
	}
	unserialize_func[serialize_default] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{i: bin}
		val.typ = "[]byte"
		return val, nil
	}
	unserialize_func[serialize_nil] = func(bin []byte) (*hashvalue, error) {
		val := &hashvalue{}
		val.typ = "nil"
		return val, nil
	}
	unserialize_func[serialize_delete] = func(bin []byte) (*hashvalue, error) {
		return nil, errors.New("delete")
	}
}

/*hash写入入口
 * key 哈希名称
 * value 需要写入的值
 * path 哈希前缀便于分类
 * expire 字段有效期
 */

//写入数据
func Hset(name string, value interface{}, path string, expire ...int64) bool {
	if len(expire) == 0 {
		expire = []int64{expire_ever}
	}
	cache := Hget(name, path)

	return cache.do_hash(value, expire[0], "hset")
}

//哈希队列操作函数
func hash_queue(value *writeHash) {
	if _, ok := hashcache_q_m.LoadOrStore(uintptr(unsafe.Pointer(value)), true); ok {
		return
	}
	queueLock.Lock()
	hashcache_q = append(hashcache_q, value)
	if len(hashcache_q) >= 5000 && len(hash_sync_chan) == 0 {
		send_queue()
	}
	queueLock.Unlock()
}
func send_queue() {
	tmp := make([]*writeHash, len(hashcache_q))
	copy(tmp, hashcache_q)
	hash_sync_chan <- tmp
	hashcache_q = hashcache_q[:0]
}

//hash读取
func Has(name string, path string) (*Hashvalue, bool) {
	v, ok := hashcache.Load(path)
	if !ok {
		return nil, false
	}
	vv, ok := v.(*sync.Map).Load(name)
	if ok {
		return vv.(*Hashvalue), true
	}
	return nil, false
}
func Hget(name string, path string) *Hashvalue {
	var value_v *Hashvalue
	var path_v *sync.Map
	path_v_i, ok := hashcache.Load(path)
	if !ok {
		path_v_i = new(sync.Map)
		hashcache.Store(path, path_v_i)
	}
	path_v = path_v_i.(*sync.Map)
	value_v_i, ok := path_v.Load(name)
	if !ok {
		value_v_i = &Hashvalue{value: new(sync.Map), path_m: path_v, writevalue: &writeHash{path: path, value: new(sync.Map), expire: expire_none, name: name}}
		path_v.Store(name, value_v_i)
	}
	value_v = value_v_i.(*Hashvalue)
	if value_v.writevalue.expire == expire_ever || value_v.writevalue.expire > time.Now().Unix() {
		return value_v
	}
	//超时，重置value_v
	value_v = &Hashvalue{value: new(sync.Map), path_m: path_v, writevalue: &writeHash{path: path, value: new(sync.Map), expire: expire_none, name: name}}
	path_v.Store(name, value_v)
	return value_v
}

//hash删除
func Hdel(name string, path string) {

	if path_v_i, ok := hashcache.Load(path); ok {
		path_v := path_v_i.(*sync.Map)
		if value_i, ok := path_v.Load(name); ok {
			value := value_i.(*Hashvalue)
			if value.writevalue.expire != expire_none {
				write := new(sync.Map)
				write.Store("0", new_hashvalue(0))
				writeString := map[string]map[string]*writeHash{path: map[string]*writeHash{name: &writeHash{name: name, path: path, expire: expire_delete_name, value: write}}}
				hash_write(writeString)
			}
			path_v.Delete(name)
		}
	}
}

//hash删除path下所有key
func Hdel_all(path string) {
	_, ok := hashcache.Load(path)
	if ok {
		hashcache.Delete(path)
		Hdel(path, "_hot")
		write := new(sync.Map)
		write.Store("0", &hashvalue{})
		writeString := map[string]map[string]*writeHash{path: map[string]*writeHash{"": &writeHash{path: path, expire: expire_delete_path, value: write}}}
		go hash_write(writeString)
	}
}

/*从单文件读取hash缓存数据
 *传入文件路径
 *当任意数据不正确时，都尝试从bak读取
 */
var no1 int

func makehashfromfile(file string, is_main bool) bool {
	b := cachebufpool.Get().(*MsgBuffer)
	b1 := cachebufpool.Get().(*MsgBuffer)
	b2 := cachebufpool.Get().(*MsgBuffer)
	f, err1 := ioutil.ReadFile(file)
	defer func() {
		cachebufpool.Put(b)
		cachebufpool.Put(b1)
		cachebufpool.Put(b2)
	}()
	if err1 != nil {
		return false
	}
	b.Reset()
	b.Write(f)
	no_back := true
	for b.Len() > 6 {
		msglen := int(binary.LittleEndian.Uint32(b.Next(4)))
		if msglen > MAXLEN || b.Len() < msglen || msglen == 0 {
			break
		}
		is_compress := b.Next(1)
		check := b.Next(4)
		msg := b.Next(msglen - 5)
		if Bytes2str(check) != Bytes2str(Crc32_check(msg)) {
			DEBUG("校验错误原始值", check, Crc32_check(msg))
			no_back = false

			continue
		}
		b2.Reset()
		if is_compress[0] == 1 {
			b2.Write(DogzipUnCompress(msg))
		} else {
			b2.Write(msg)
		}
		var l uint16
		binary.Read(b2, binary.LittleEndian, &l)
		path := string(b2.Next(int(l)))
		l = 0
		binary.Read(b2, binary.LittleEndian, &l)
		key := string(b2.Next(int(l)))
		t := int64(binary.LittleEndian.Uint64(b2.Next(8)))
		if t == expire_delete_name {
			path_v_i, ok := hashcache.Load(path)
			if ok {
				path_v := path_v_i.(*sync.Map)
				path_v.Delete(key)
			}
			continue
		}

		if t == expire_delete_path {
			_, ok := hashcache.Load(path)
			if ok {
				hashcache.Delete(path)
			}
			continue
		}
		if t != expire_ever && t < time.Now().Unix() {
			continue
		}

		va := Hget(key, path)
		v32 := uint32(0)
		binary.Read(b2, binary.LittleEndian, &v32)
		b1.Reset()
		b1.Write(b2.Next(int(v32)))
		for b1.Len() > 0 {
			l = 0
			binary.Read(b1, binary.LittleEndian, &l)
			kk := string(b1.Next(int(l)))
			v32 = 0
			binary.Read(b1, binary.LittleEndian, &v32)
			r_v, err := unserialize(b1.Next(int(v32)))

			if err == nil && err1 == nil {
				va.Store(kk, r_v)
			} else if err.Error() == "delete" {
				va.Delete(kk)
			} else {
				//初次出现err，尝试从副本中读取指定数据
				no_back = false
				if !is_main {
					DEBUG("读取持久化文件失败,反序列化失败的path:" + key + " key:" + path + " map[" + kk + "]原始值:")
				}
			}

		}

		va.writevalue.expire = t
		if t > 0 {
			if v, ok := hashdelete.Load(t); ok {
				h := v.([]map[string]string)

				h = append(h, map[string]string{"name": va.writevalue.name, "path": va.writevalue.path})
			} else {
				hashdelete.Store(t, []map[string]string{map[string]string{"name": va.writevalue.name, "path": va.writevalue.path}})
			}
		}
		path_i, _ := hashcache.Load(path)
		path_i.(*sync.Map).Store(key, va)
		continue

	}
	return no_back
}

func init() {
	if ok, _ := PathExists(CACHE_Path); !ok {
		err := os.Mkdir(CACHE_Path, os.ModePerm)
		if err != nil {
			fmt.Printf("cache新建文件夹失败会导致缓存无法持久化![%v]\n", err)
		}

	}
	for i := 0; i < runtime.NumCPU(); i++ {
		uncompress_chan <- new(bytes.Buffer)
		gzip_writer := new(Gzip_writer)
		gzip_writer.Buf = new(MsgBuffer)
		gzip_writer.Writer, _ = gzip.NewWriterLevel(gzip_writer.Buf, 6)
		gzipcompress_chan <- gzip_writer
	}
	init_unserialize_func() //反序列化方法初始化

	hashcache_q = make([]*writeHash, 0, 5000)
	ok := makehashfromfile(CACHE_Path+"/"+CACHE_FILE_NAME, true) //加载持久化缓存
	if !ok {
		ok = makehashfromfile(CACHE_Path+"/"+CACHE_FILE_NAME+".bak", false) //尝试从bak文件加载
		if ok {
			CopyFile(CACHE_Path+"/"+CACHE_FILE_NAME+".bak", CACHE_Path+"/"+CACHE_FILE_NAME)
		}
	}

	go func() {
		for true {
			//延时1000毫秒执行删除
			now := time.Now()
			t := now.Unix()
			time.Sleep(time.Millisecond * time.Duration(now.UnixNano()/1e6-t*1000))
			if v, ok := hashdelete.Load(t); ok {
				go func(m []map[string]string) {
					if len(m) > 0 {
						for _, v := range m {
							path_v_i, ok := hashcache.Load(v["path"])
							if ok {
								path := path_v_i.(*sync.Map)
								path.Delete(v["name"])
							}
						}
					}
					hashdelete.Delete(t)
				}(v.([]map[string]string))
			}
			if t%3600 == 0 && now.Hour() == 4 {
				go hash_write_db()
			}
			queueLock.Lock()
			send_queue()
			queueLock.Unlock()
		}
	}()
	go hash_sync()
}
func hash_sync() {
	for signal := range hash_sync_chan {

		if signal == nil {
			break
		} else {
			syncHash(signal)
		}
	}
}
func syncHash(signal []*writeHash) {
	writehashLock.Lock()
	defer func() {
		for _, v := range signal {
			hashcache_q_m.Delete(uintptr(unsafe.Pointer(v)))
		}
		writehashLock.Unlock()
		if err := recover(); err != nil {
			DEBUG(err)
		}
	}()
	if len(signal) == 0 {
		return
	}
	write := make(map[string]map[string]*writeHash, len(signal))
	for _, v := range signal { //分别将队列取出执行hash写入同步
		if v == nil || v.value == nil {
			continue
		}
		if write[v.path] == nil {
			write[v.path] = make(map[string]*writeHash)
		}
		if write[v.path][v.name] == nil {
			write[v.path][v.name] = v
		} else {
			v.value.Range(func(k1, v1 interface{}) bool {
				write[v.path][v.name].value.Store(k1, v1)
				return true
			})
		}
		write[v.path][v.name].expire = v.expire
	}
	if len(write) > 0 {
		go hash_write(write)
	}

}
func Destroy() {
	fmt.Println("正在退出，请等待缓存写入硬盘")
	send_queue()
	hash_sync_chan <- nil
	fmt.Println("已经保存完毕，如果程序还不退出，请再按crtl+c或者强制关闭进程")
}

/**
 * 以下内容是队列
 **/

var (
	l_q        sync.Mutex //队列锁
	list_chans map[string][]chan int
	list_cache *Hashvalue //队列保存变量
)

func list_init() {
	list_chans = make(map[string][]chan int)
	list_cache = Hget("list", "_cache")
	//持久化list缓存
	go func() {
		for true {
			time.Sleep(time.Second * SAVE_TIME)
			list_cache.Save()
		}
	}()
	//list_cache = new(Hashvalue)
	//llen_test()
}

/**
*将一个或多个值插入到列表的尾部(最右边)。
*插入一个值Rpush("mylist","hello")
*插入多个值Rpush("mylist","1","2","3")
*插入[]interface{}切片:
   var list []interface{}
   list = append(list,"1")
   list = append(list,map[string]string{"name":"luyu"})
   list = append(list,100)
   Rpush("mylist",list...)
**/
func RPUSH(key string, list ...string) bool {
	if len(list) == 0 {
		return false
	}
	l_q.Lock()

	var l []string
	list_cache.Get(key, &l)
	l = append(l, list...)
	if len(list_chans[key]) > 0 {
		var new_chans []chan int
		out := true
		//整理空chan，以及对第一个正在等待的chan进行解锁
		for _, list_chan := range list_chans[key] {
			if len(list_chan) > 0 {
				if out {
					<-list_chan
					out = false
				} else {
					new_chans = append(new_chans, list_chan)
				}
			}
		}
		list_chans[key] = new_chans
	}
	list_cache.Store(key, l)
	l_q.Unlock()
	return true
}

/**
 *将一个或多个值插入到列表的头部(最左边)，用法同Rpush
 **/
func LPUSH(key string, list ...string) bool {
	if len(list) == 0 {
		return false
	}
	l_q.Lock()
	cache, _ := list_cache.Load(key)
	var l []string
	if cache != nil {
		l = cache.([]string)
	}
	l = append(list, l...)
	if len(list_chans[key]) > 0 {
		var new_chans []chan int
		out := true
		//整理空chan，以及对第一个正在等待的chan进行解锁
		for _, list_chan := range list_chans[key] {
			if len(list_chan) > 0 {
				if out {
					<-list_chan
					out = false
				} else {
					new_chans = append(new_chans, list_chan)
				}
			}
		}
		list_chans[key] = new_chans
	}
	list_cache.Store(key, l)
	l_q.Unlock()
	return true
}

/**
 *取出指定列表的第一个元素，如果列表没有元素会阻塞列表直到等待超时或发现可弹出元素为止。
 *LPOP(list1,100)取出名字为list1的列表，没有会等待100秒
 *LPOP(list1)取出列表,没有直接返回
 *当ok返回值为false，则为超时取队列失败
 */
func LPOP(key string, timeout ...int) (result string, ok bool) {
	l_q.Lock()
	var l []string
	list_cache.Get(key, &l)
	defer func(l []string) {
		if len(l) > 0 {
			if len(l) == 1 {
				list_cache.Delete(key)
			} else {
				l = l[1:]
				list_cache.Store(key, l)
			}
		}
		l_q.Unlock()
	}(l)
	if len(l) > 0 {
		result = l[0]
		ok = true
		return
	} else {
		list_chan := make(chan int, 1)
		list_chans[key] = append(list_chans[key], list_chan)
		//加塞
		list_chan <- 0
		l_q.Unlock()
		if len(timeout) == 1 {
			ok = true
			result = waitchan(key, &ok, timeout[0], list_chan)
		}
		l_q.Lock()
	}
	return
}

/**
 *取出指定列表的最后一个元素，如果列表没有元素会阻塞列表直到等待超时或发现可弹出元素为止。
 *RPOP(list1,100)取出名字为list1的列表，没有会等待100秒
 *RPOP(list1)取出列表,没有直接返回
 *当ok返回值为false，则为超时失败
 */
func RPOP(key string, timeout ...int) (result string, ok bool) {
	l_q.Lock()
	var l []string
	list_cache.Get(key, &l)
	defer func(l []string) {
		if len(l) > 0 {
			if len(l) == 1 {
				list_cache.Delete(key)
			} else {
				l = l[1:]
				list_cache.Store(key, l)
			}
		}
		l_q.Unlock()
	}(l)
	if len(l) > 0 {
		ok = true
		result = l[len(l)-1]
		return
	} else {
		list_chan := make(chan int, 1)
		list_chans[key] = append(list_chans[key], list_chan)
		//加塞
		list_chan <- 0
		l_q.Unlock()
		if len(timeout) == 1 {
			ok = true
			result = waitchan(key, &ok, timeout[0], list_chan)
		}
		l_q.Lock()
	}
	return
}

func waitchan(key string, ok *bool, timeout int, list_chan chan int) (result string) {
	go func(list_chan chan int) {
		//等待指定时间
		time.Sleep(time.Second * time.Duration(timeout))
		l_q.Lock()
		//超时返回nil与false
		*ok = false
		//解锁
		if len(list_chan) > 0 {
			<-list_chan
		}
		l_q.Unlock()
	}(list_chan)
	//尝试解锁
	list_chan <- 0
	l_q.Lock()
	defer l_q.Unlock()

	var l []string
	list_cache.Get(key, &l)
	if len(l) > 0 {
		result = l[0]
	}
	//释放阻塞
	<-list_chan
	close(list_chan)
	return
}

/**
 * 通过索引获取队列的元素
 * 获取失败返回nil,false
 **/
func LINDEX(key string, index int) (result string, ok bool) {
	l_q.Lock()
	defer l_q.Unlock()
	var l []string
	list_cache.Get(key, &l)
	if len(l) < index {
		return
	}
	return l[index], true
}

/**
 * 获取列表长度
 **/
func LLEN(key string) int {
	l_q.Lock()
	defer l_q.Unlock()
	var l []string
	list_cache.Get(key, &l)
	return len(l)
}

/**
 * 获取列表指定范围内的元素，起始元素是0
 * 表不存在返回false
 * LRANGE("list",2,3)取第2到3个元素
 * LRANGE("list",5,2)如果start比stop小,调换他们的顺序，取第2到第5个元素
 * LRANGE("list",-2,1)取第1个到倒数第2个元素,假如10个元素，等同于1,8
 * LRANGE("list",2)如果stop为空，则取第0到2个元素
 * LRANGE("list",-3) 取最后3个元素
 * 假如stop超过列表长度，返回空
 **/
func LRANGE(key string, start int, param ...int) ([]string, bool) {
	l_q.Lock()
	defer l_q.Unlock()
	var stop int
	cache, _ := list_cache.Load(key)
	var l []string
	if cache != nil {
		l = cache.([]string)
	} else {
		return nil, false
	}
	if len(param) == 0 {
		if start > 0 {
			stop = 0
		} else {
			stop = len(l) - 1
		}
	} else {
		stop = param[0]
	}
	if start < 0 {
		start = len(l) + start
		if start < 0 {
			start = 0
		}
	}

	if stop < 0 {
		stop = len(l) + stop
		if stop < 0 {
			stop = 0
		}
	}
	s := start
	if start > stop {
		start = stop
		stop = s
	}
	//最大值超过最大长度,返回最大长度
	if stop > len(l)-1 {
		stop = len(l) - 1
	}
	//起始大于最大长度,返回空
	if start > len(l)-1 {
		return nil, true
	}
	result := l[start:]
	return result[:stop+1-start], true
}

/**
 *根据参数 COUNT 的值，移除列表中与参数 VALUE 相等的元素。
 *count > 0 : 从表头开始向表尾搜索，移除与 VALUE 相等的元素，数量为 COUNT 。
 *count < 0 : 从表尾开始向表头搜索，移除与 VALUE 相等的元素，数量为 COUNT 的绝对值。
 *count = 0 : 移除表中所有与 VALUE 相等的值。
 */
func LREM(key string, count int, value string) bool {
	l_q.Lock()
	defer l_q.Unlock()
	cache, _ := list_cache.Load(key)
	var l []string
	if cache != nil {
		l = cache.([]string)
	} else {
		return false
	}
	var new_list []string
	length := count
	if length < 0 {
		length = length * -1
	}
	if count == 0 {
		for k, v := range l {
			if v != value {
				new_list = append(new_list, l[k])
			}
		}
	} else if count > 0 {
		for k, v := range l {
			if v != value {
				new_list = append(new_list, l[k])
			} else {
				length--
				if length < 0 {
					new_list = append(new_list, l[k])
				}

			}
		}
	} else if count < 0 {
		for kk, _ := range l {
			k := len(l) - kk - 1
			if l[k] != value {
				new_list = append([]string{l[k]}, new_list...)
			} else {
				length--
				if length < 0 {
					new_list = append([]string{l[k]}, new_list...)
				}
			}
		}
	}
	l = new_list
	list_cache.Store(key, l)
	return true
}

/**
 * LTRIM 对一个列表进行修剪(trim)，就是说，让列表只保留指定区间内的元素，不在指定区间之内的元素都将被删除。
 * start 与 stop定义参照LRANGE
 * 设置超过最大值的start会清空列表
 * 设置超过最大值的stop等同于最大值
 **/
func LTRIM(key string, start int, param ...int) bool {
	l_q.Lock()
	defer l_q.Unlock()
	cache, _ := list_cache.Load(key)
	var l []string
	if cache != nil {
		l = cache.([]string)
	} else {
		return false
	}
	var stop int
	if len(param) == 0 {
		if start > 0 {
			stop = 0
		} else {
			stop = len(l) - 1
		}
	} else {
		stop = param[0]
	}
	if start < 0 {
		start = len(l) + start
		if start < 0 {
			start = 0
		}
	}

	if stop < 0 {
		stop = len(l) + stop
		if stop < 0 {
			stop = 0
		}
	}
	s := start
	if start > stop {
		start = stop
		stop = s
	}
	//最大值超过最大长度,等同于最大值
	if stop > len(l)-1 {
		stop = len(l) - 1
	}
	//起始大于最大长度,清空列表
	if start > len(l)-1 {
		l = nil
		return true
	}
	result := l[start:]
	l = result[:stop+1-start]
	list_cache.Store(key, l)
	return true
}

func pop_test() {
	begin := time.Now().Unix() + 1
	DEBUG("开始测试")
	//读取左边数据等待100秒
	//线程1
	go func() {
		DEBUG(LPOP("test", 100))
		DEBUG("1等待了", time.Now().Unix()-begin, "秒")
	}()
	//延迟1秒执行线程2
	time.Sleep(time.Second * 1)
	//线程2
	go func() {
		DEBUG(LPOP("test", 10))
		DEBUG("2等待了", time.Now().Unix()-begin, "秒")
	}()
	//等5秒后再写入
	time.Sleep(time.Second * 5)
	DEBUG("开始写入1")
	RPUSH("test", "久等了")
	//等待3秒后写入
	time.Sleep(time.Second * 3)
	DEBUG("开始写入2")
	LPUSH("test", "第二次写入")
}

func lrange_test() {
	LPUSH("test", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9")
	DEBUG(LRANGE("test", 0, 1))  //第0到第1个，[0 1]
	DEBUG(LRANGE("test", 5, 10)) //[5 6 7 8 9]
	DEBUG(LRANGE("test", -2))    //最后2个，[8 9]
}

func lrem_test() {
	LPUSH("test", "5", "2", "2", "3", "3", "3", "4", "5", "6", "7")
	LREM("test", 0, "2")           //去掉所有的2
	DEBUG(list_cache.Load("test")) //[5 3 3 3 4 5 6 7]
	LREM("test", 2, "3")           //去掉左边两个3
	DEBUG(list_cache.Load("test")) //[5 3 4 5 6 7]
	LREM("test", -1, "5")          //去掉右边那个5
	DEBUG(list_cache.Load("test")) //[5 3 4   6 7]
}

func ltrim_test() {
	LPUSH("test", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9")
	DEBUG(LTRIM("test", 0, 7))
	DEBUG(list_cache.Load("test")) //[0 1 2 3 4 5 6 7]
	DEBUG(LTRIM("test", 2, 4))
	DEBUG(list_cache.Load("test")) //[2 3 4]
	DEBUG(LTRIM("test", 10))
	DEBUG(list_cache.Load("test")) //[2 3 4]
}

func llen_test() {
	LPUSH("test", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0")
	limit := LLEN("test")
	DEBUG(limit)
	for i := 0; i < limit; i++ {
		LPOP("test")
		DEBUG(LLEN("test"))
	}

}

func DEBUG(v ...interface{}) {
	if ISDEBUG {
		_, file, line, ok := runtime.Caller(1)
		if ok {
			v = append([]interface{}{fmt.Sprintf("%s,line %d:", file, line)}, v...)
		}
		fmt.Println(v...)
	}
}
func Log(format string, v ...interface{}) {
	_, file, line, ok := runtime.Caller(1)
	if ok {
		v = append([]interface{}{fmt.Sprintf("%s,line %d:", file, line)}, v...)
	}
	fmt.Printf("%s "+format+"\r\n", v...)
}
func Bytes2str(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

//gzip压缩
func DogzipCompress(src []byte) []byte {
	g := <-gzipcompress_chan
	defer func() {
		g.Buf.Reset()
		gzipcompress_chan <- g
	}()
	g.Writer.Reset(g.Buf)
	leng, err := g.Writer.Write(src)
	if err != nil || leng == 0 {
		return nil
	}
	err = g.Writer.Flush()
	if err != nil {
		return nil
	}
	err = g.Writer.Close()
	if err != nil {
		return nil
	}
	b := make([]byte, len(g.Buf.Bytes()))
	copy(b, g.Buf.Bytes())
	return b
}

//进行gzip解压缩
func DogzipUnCompress(compressSrc []byte) []byte {
	b := <-uncompress_chan
	defer func() {
		uncompress_chan <- b
	}()
	b.Reset()
	b.Write(compressSrc)
	r, err := gzip.NewReader(b)
	if err != nil {
		return nil
	}
	defer r.Close()
	ndatas, err := ioutil.ReadAll(r)
	res := make([]byte, len(ndatas))
	copy(res, ndatas)
	if err != nil {
		return res
	}
	return res
}
func CopyFile(srcName, dstName string) (written int64, err error) {

	src, err := os.Open(srcName)
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.OpenFile(dstName, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return
	}
	defer dst.Close()
	return io.Copy(dst, src)
}

// 判断文件夹是否存在
func PathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
