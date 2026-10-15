package main

import "fmt"


func main(){
	arr:=[4]int{1, 2, 3, 4}
	fmt.Print(arr)
	for _,value := range arr{
		fmt.Print(value)

	}
}