package main

import "fmt"

func main() {
	fmt.Println("hello Go")
	
	var a int = 80
	var foo int = 40
	var baz, bar int = 42, 18      // 修复：将 foo 改为 baz，避免重复声明
	var foo2 = 42                  // 修复：将 foo 改为 foo2，避免重复声明
	const constant = "this is a day" // 修复：修正 constant 拼写

	const(
		
	)
	// 修复：使用所有声明的变量，消除 "declared and not used" 编译错误
	fmt.Println(a, foo, baz, bar, foo2, constant)
}