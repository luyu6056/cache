package cache

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func StartWebServer(ipPort string) {

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		buf := NewBuffer(4096)
		var pathlist cacheSort
		hashcache.Range(func(key, path_i interface{}) bool {
			pathname := key.(string)
			pathlist = append(pathlist, kv{pathname, path_i})
			return true
		})
		bg := 0
		sort.Sort(pathlist)
		for _, v := range pathlist {
			pathname := v.key
			path := v.v.(*sync.Map)
			var cachelist cacheSort
			path.Range(func(k, i interface{}) bool {
				key := k.(string)
				cachelist = append(cachelist, kv{key, i})
				return true
			})
			sort.Sort(cachelist)
			for _, v := range cachelist {
				key := v.key
				c := v.v.(*Hashvalue)
				num := 0
				bg++
				var valuelist cacheSort
				c.value.Range(func(k, i interface{}) bool {
					num++
					valuelist = append(valuelist, kv{k.(string), i})
					return true
				})
				n := 0
				sort.Sort(valuelist)
				for _, v := range valuelist {
					n++
					class := ""
					if bg%2 == 0 {
						class = `btbg`
					}
					buf.WriteString("<tr>")
					if n == 1 {
						buf.WriteString("<td class=\"" + class + "\" rowspan=" + strconv.Itoa(num) + ">")
						buf.WriteString(pathname)
						buf.WriteString("</td>")
					}
					if n == 1 {
						buf.WriteString("<td class=\"" + class + "\" rowspan=" + strconv.Itoa(num) + ">")
						buf.WriteString(key)
						buf.WriteString("</td>")
					}

					buf.WriteString("<td class=\"" + class + "\">")
					buf.WriteString(v.key)
					buf.WriteString("</td>")
					buf.WriteString("<td class=\"" + class + "\">")
					buf.WriteString(v.v.(*hashvalue).typ)
					buf.WriteString("</td>")
					buf.WriteString("<td class=\"" + class + "\" style=\"overflow:hidden;white-space:nowrap;\" title=\"" + strings.ReplaceAll(v.v.(*hashvalue).str, `"`, `&quot;`) + "\">")
					buf.WriteString(v.v.(*hashvalue).str)
					buf.WriteString("</td>")
					if n == 1 {
						buf.WriteString("<td class=\"" + class + "\" rowspan=" + strconv.Itoa(num))
						if c.writevalue.expire > 0 {
							buf.WriteString(" title=\"" + time.Unix(c.writevalue.expire, 0).Format("2006-01-02 15:04:05") + "\">")
							buf.WriteString(strconv.Itoa(int(c.writevalue.expire - now.Unix())))
						} else if c.writevalue.expire == -1 {

							buf.WriteString(">永不超时")
						} else {
							buf.WriteString(">")
							buf.WriteString(strconv.Itoa(int(c.writevalue.expire)))
						}
					}
					buf.WriteString("</td>")
					buf.WriteString("</tr>")

				}
			}

		}

		w.Write([]byte(`<!DOCTYPE html>
<html>
	<head>
		<meta http-equiv="X-UA-Compatible" content="IE=edge,chrome=1">
		<meta http-equiv="content-type" content="text/html;charset=utf-8">
		<title>缓存管理</title>
		<style>` + css + `</style>
	</head>
	<body>
<table width="100%" border="0" cellspacing="1" cellpadding="4" bgcolor="#cccccc" class="tabtop13" align="center">
<tr>
  <td width="200px" class="btbg font-center titfont" >path</td>
    <td width="200px" class="btbg font-center titfont">name</td>
    <td width="200px" class="btbg font-center titfont" >key</td>
    <td width="80px" class="btbg font-center titfont" >type</td>
    <td  class="btbg font-center titfont" >string值</td>
 
    <td width="80px" class="btbg font-center titfont" >超时</td>
  </tr>
` + buf.String() + `
</table>
	</body>
</html>
`))
	})
	go http.ListenAndServe(ipPort, nil)
}

const (
	css = `@charset "utf-8";
/* CSS Document */
table{table-layout:fixed;word-break:break-all;}
.tabtop13 {
	margin-top: 13px;
}
.tabtop13 td{
	background-color:#ffffff;
	height:25px;
	line-height:150%;
}
.font-center{ text-align:center}
.btbg{background:#e9faff !important;}
.btbg1{background:#f2fbfe !important;}
.btbg2{background:#f3f3f3 !important;}
.biaoti{
	font-family: 微软雅黑;
	font-size: 26px;
	font-weight: bold;
	border-bottom:1px dashed #CCCCCC;
	color: #255e95;
}
.titfont {
	
	font-family: 微软雅黑;
	font-size: 16px;
	font-weight: bold;
	color: #255e95;
	background: url(../images/ico3.gif) no-repeat 15px center;
	background-color:#e9faff;
}
.tabtxt2 {
	font-family: 微软雅黑;
	font-size: 14px;
	font-weight: bold;
	text-align: right;
	padding-right: 10px;
	color:#327cd1;
}
.tabtxt3 {
	font-family: 微软雅黑;
	font-size: 14px;
	padding-left: 15px;
	color: #000;
	margin-top: 10px;
	margin-bottom: 10px;
	line-height: 20px;
}`
)

type kv struct {
	key string
	v   interface{}
}
type cacheSort []kv

func (l cacheSort) Len() int {
	return len(l)
}
func (l cacheSort) Less(i, j int) bool {
	numi, erri := strconv.Atoi(l[i].key)
	numj, errj := strconv.Atoi(l[j].key)
	if erri == nil && errj == nil {
		return numi < numj
	}
	return l[i].key < l[j].key
}
func (l cacheSort) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}
