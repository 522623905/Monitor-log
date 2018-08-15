慕课网视频的一个日志监控实例整理

log_process主程序
mock_data模拟往access.log写输入

安装fluxdb并运行:
	sudo docker pull tutum/influxdb
	sudo docker run -d -p 8083:8083 -p8086:8086 --expose 8090 --expose 8099 --name influxsrv tutum/influxdb
创建数据库:
	...
浏览器输入:
	http://127.0.0.1:8083/
	创建数据库:CREATE DATABASE "imooc"

安装grafana:
	docker run -d --name=grafana -p 3000:3000 grafana/grafana
浏览器登录localhost:3000:
	配置DATA Sources为influxdb,才能读取到influxdb的数据

运行:
	./mock_data
	./log_process -path ./access.log -influxDsn http://127.0.0.1:8086@imooc@imoocpass@imooc@s

浏览器打开:
	localhost:3000 可查看监控的图表
	
http可查看程序的部分监控信息:
	curl 127.0.0.1:9193/monitor 