package main

import (
	"strings"
	"testing"
)

// rewriteDisks 复刻 buildTargetXML 里的磁盘改写逻辑，便于脱离 virsh 单测。
func rewriteDisks(xmlText, dir string) (out string, moved, ejected []string) {
	out = reDiskBlock.ReplaceAllStringFunc(xmlText, func(block string) string {
		if strings.Contains(block, "device='cdrom'") || strings.Contains(block, `device="cdrom"`) ||
			strings.Contains(block, "device='floppy'") || strings.Contains(block, `device="floppy"`) {
			if m := reSourceFile.FindStringSubmatch(block); m != nil {
				ejected = append(ejected, m[1])
				block = reSourceElem.ReplaceAllString(block, "")
			}
			return block
		}
		return reSourceFile.ReplaceAllStringFunc(block, func(match string) string {
			old := reSourceFile.FindStringSubmatch(match)[1]
			next := pathJoin(dir, baseOf(old))
			if old == next {
				return match
			}
			moved = append(moved, old+" → "+next)
			return "<source file='" + next + "'"
		})
	})
	return out, moved, ejected
}

const domainXML = `  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' discard='unmap' detect_zeroes='unmap'/>
      <source file='/var/lib/kvm-storage/data1/test2.qcow2' index='2'/>
      <backingStore/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/opt/kvm/server/data/iso/alpine-virt.iso'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/kvm-storage/data1/data.qcow2'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
  </devices>`

func TestRewriteDiskDir(t *testing.T) {
	got, moved, _ := rewriteDisks(domainXML, "/opt/vm-disks")
	if !strings.Contains(got, "/opt/vm-disks/test2.qcow2") {
		t.Fatalf("第一块盘未改写: %s", got)
	}
	if !strings.Contains(got, "/opt/vm-disks/data.qcow2") {
		t.Fatalf("第二块盘未改写: %s", got)
	}
	if strings.Contains(got, "kvm-storage") {
		t.Fatalf("旧路径残留: %s", got)
	}
	// driver type 必须保留：目标端按它决定建什么格式的盘，
	// 丢了就会退回池默认（raw），报 "Image is not in qcow2 format"。
	if !strings.Contains(got, "type='qcow2'") {
		t.Fatalf("driver type 被丢弃: %s", got)
	}
	if !strings.Contains(got, "index='2'") {
		t.Fatalf("index 属性被丢弃: %s", got)
	}
	if len(moved) != 2 {
		t.Fatalf("改写盘数应为 2，得到 %d: %v", len(moved), moved)
	}
}

// 光驱：设备保留（sda/sata/readonly 都在），只把 <source> 摘掉。
// 否则目标端会报 Cannot access storage file '...iso': No such file or directory。
func TestCDROMMediumEjected(t *testing.T) {
	got, _, ejected := rewriteDisks(domainXML, "/opt/vm-disks")
	if len(ejected) != 1 || ejected[0] != "/opt/kvm/server/data/iso/alpine-virt.iso" {
		t.Fatalf("弹出记录不对: %v", ejected)
	}
	if strings.Contains(got, "alpine-virt.iso") {
		t.Fatalf("ISO 路径仍残留在 XML 里: %s", got)
	}
	for _, want := range []string{"device='cdrom'", "dev='sda'", "bus='sata'", "<readonly/>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("光驱设备定义被破坏，缺少 %s:\n%s", want, got)
		}
	}
}

// 目标目录末尾带斜杠时不能拼出双斜杠。
func TestRewriteDiskTrailingSlash(t *testing.T) {
	got, _, _ := rewriteDisks(domainXML, "/opt/vm-disks/")
	if strings.Contains(got, "/opt/vm-disks//") {
		t.Fatalf("出现双斜杠: %s", got)
	}
}

// 源路径已经在目标目录里时不应算作改写。
func TestRewriteDiskNoopWhenSameDir(t *testing.T) {
	_, moved, _ := rewriteDisks(domainXML, "/var/lib/kvm-storage/data1")
	if len(moved) != 0 {
		t.Fatalf("路径未变化时不应记录改写: %v", moved)
	}
}

func TestPathHelpers(t *testing.T) {
	if baseOf("/opt/a/b.qcow2") != "b.qcow2" {
		t.Fatalf("baseOf 错误: %s", baseOf("/opt/a/b.qcow2"))
	}
	if pathJoin("/opt/vm-disks/", "b.qcow2") != "/opt/vm-disks/b.qcow2" {
		t.Fatalf("pathJoin 错误: %s", pathJoin("/opt/vm-disks/", "b.qcow2"))
	}
	if pathJoin("/opt/vm-disks", "b.qcow2") != "/opt/vm-disks/b.qcow2" {
		t.Fatalf("pathJoin 无尾斜杠错误: %s", pathJoin("/opt/vm-disks", "b.qcow2"))
	}
}

func TestSanitizeName(t *testing.T) {
	if got := sanitizeName("test2"); got != "test2" {
		t.Fatalf("得到 %s", got)
	}
	if got := sanitizeName("a/b c"); got != "a_b_c" {
		t.Fatalf("得到 %s", got)
	}
}

// 存储池路径解析：目标机真实的两类池。
func TestPoolPathMatch(t *testing.T) {
	cases := []struct {
		name, xml string
		want      string
	}{
		{"vm-disks", `<pool type='dir'><name>vm-disks</name><target><path>/opt/vm-disks</path></target></pool>`, "/opt/vm-disks"},
		{"data1", `<pool type='dir'><name>data1</name><target><path>/var/lib/kvm-storage/data1</path><permissions><mode>0755</mode></permissions></target></pool>`, "/var/lib/kvm-storage/data1"},
	}
	for _, c := range cases {
		m := rePoolPath.FindStringSubmatch(c.xml)
		if m == nil || strings.TrimSpace(m[1]) != c.want {
			t.Fatalf("%s 路径解析错误: %v", c.name, m)
		}
	}
	if m := rePoolPath.FindStringSubmatch(`<pool type='dir'><name>empty</name></pool>`); m != nil {
		t.Fatalf("没有 target 的池不应匹配到路径: %v", m)
	}
}

// 目录归属判定：等于池路径或位于其下才算，前缀相近但不同层不算。
func TestPoolDirBelongs(t *testing.T) {
	poolPath := "/opt/vm-disks"
	belongs := func(dir string) bool {
		dir = strings.TrimRight(dir, "/")
		return dir == poolPath || strings.HasPrefix(dir, poolPath+"/")
	}
	for dir, want := range map[string]bool{
		"/opt/vm-disks":              true,
		"/opt/vm-disks/":             true,
		"/opt/vm-disks/sub":          true,
		"/opt/vm-disk":               false,
		"/opt":                       false,
		"/var/lib/kvm-storage/data1": false,
	} {
		if got := belongs(dir); got != want {
			t.Fatalf("%s → 得到 %v，期望 %v", dir, got, want)
		}
	}
}
