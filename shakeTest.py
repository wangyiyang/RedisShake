import redis
import time
import random

r1=redis.Redis(host='127.0.0.1',port=6380,db=0)
r2=redis.Redis(host='127.0.0.1',port=6381,db=0)



if __name__ == '__main__':
	result = {}
	for i in range(100000):
		key = str(random.randint(1, 20000))
		value = str(random.randint(1, 20000))
		r1.set(key, value)
		result[key] = value
	# time.sleep(60)

	# for key, value in result.items():
	# 	if r2.get(key) != value:
	# 		print "Error: r2 "+ key + " is " + r2.get(key) +" != " + value;

	# print "OK!"