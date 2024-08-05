---
# Feel free to add content and custom Front Matter to this file.
# To modify the layout, see https://jekyllrb.com/docs/themes/#overriding-theme-defaults

layout: home
---
# Optimizing Tail Latency in a Heterogeneous Environment with Istio, Envoy, and a Custom Kubernetes Operator

## Introduction

Running microservices in a Kubernetes environment often involves dealing with various hardware generations and CPU architectures. In our infrastructure, we observed high tail latency in some of our services despite using Istio and Envoy as our service mesh. This article details our journey in identifying the root cause of this issue and implementing a custom solution using a Kubernetes operator to optimize tail latency.

## Identifying the Challenge

We run multiple hardware generations and different CPU architectures within our Kubernetes clusters. Our service mesh, composed of Istio for control and Envoy for the data plane, uses the LEAST_REQUEST load-balancing algorithm to distribute traffic between services. However, we noticed that certain services experienced significantly high tail latency. Upon investigation, we discovered that the disparities in hardware capabilities were the main cause of this issue.

## Understanding the Problem

Tail latency matters because it represents the latency experienced by the slowest requests, typically measured at the 95th, 99th, or 99.9th percentile. High tail latency can negatively impact user experience and indicate underlying performance bottlenecks. The default load balancing strategy in Envoy works well in homogeneous environments but struggles when hardware performance is uneven, leading to inefficient request distribution and high tail latency.

## Developing the Solution

To address this problem, we developed a custom Kubernetes operator. This operator dynamically adjusts the load balancing weights of Envoy proxies using Istio's CRD called ServiceEntry. Here's how we implemented our solution:

### Step 1: Measuring the CPU Utilization of the Pods

We deployed a dedicated VictoriaMetrics cluster to collect real-time CPU usage statistics for each pod. Our operator interfaces with the VictoriaMetrics API to gather this data, calculating the average CPU usage for each service by aggregating individual pod metrics.

### Step 2: Calculating Weight Adjustments

Based on the average CPU usage, the operator determines the "distance" of each pod's CPU usage from the average. Pods with CPU usage below the average are assigned higher weights, indicating they can handle more requests. Conversely, pods with higher-than-average CPU usage receive lower weights to prevent them from becoming bottlenecks.

### Step 3: Applying the Weights

The calculated weights are applied to the Envoy proxies via Istio's ServiceEntry resources. This dynamic adjustment ensures that request distribution considers each pod's real-time performance, optimizing load balancing to reduce tail latency.

## Results and Impact

To effectively evaluate the impact of our optimization strategy, we conducted extensive testing using a set of 15 Nginx pods, each executing a Lua script to calculate different Fibonacci numbers ranging from 25 to 29. We used Fortio to generate load at a rate of 1,500 requests per second (rps). Below is the Lua code employed for each Nginx pod:
```
error_log /dev/stdout info;
lua_shared_dict my_dict 1m;

init_by_lua_block {
   -- Initialize a shared dictionary to store the random Fibonacci input
   local dict = ngx.shared.my_dict
   -- Seed the random number generator
   math.randomseed(os.time() + ngx.worker.pid())
   -- Generate a random number (could adjust the range as needed, here we choose between 20 and 30)
   local random_fib_n = math.random(25, 29)
   -- Store the random number in the shared dictionary
   dict:set("fib_n", random_fib_n)
   -- Log the selected random_fib_n value at initialization
   ngx.log(ngx.INFO, "Initialized with random_fib_n: " .. random_fib_n)             
}      
server {
   listen       80;
   server_name  localhost;

   location / {
       default_type 'text/plain';
       content_by_lua_block {
         -- Function to calculate Fibonacci
         local function fib(n)
             if n<2 then return n end
             return fib(n-1)+fib(n-2)
         end

         -- Fetch the Fibonacci input from the shared dictionary
         local dict = ngx.shared.my_dict
         local n = dict:get("fib_n")
         -- Compute the Fibonacci number
         local fib_number = fib(n)
         -- Output the result
         ngx.say(fib_number)
         ngx.say(n)
         -- Log the Fibonacci number and the value of n
         ngx.log(ngx.INFO, "This nginx calculated fibonacci number for n=" .. n)
       }
   }

   error_page   500 502 503 504  /50x.html;
   location = /50x.html {
       root   /usr/share/nginx/html;
   }
}

```

### Results Before Optimization

Pod List and Calculated Fibonacci Numbers:
- sleep-lior-2-6794d4cfdc-2gs9b: 25
- sleep-lior-2-6794d4cfdc-6r6lg: 25
- sleep-lior-2-6794d4cfdc-7rrwr: 27
- sleep-lior-2-6794d4cfdc-gv856: 27
- sleep-lior-2-6794d4cfdc-jgxqg: 26
- sleep-lior-2-6794d4cfdc-jz462: 27
- sleep-lior-2-6794d4cfdc-kr64w: 27
- sleep-lior-2-6794d4cfdc-kxhwx: 27
- sleep-lior-2-6794d4cfdc-m2xcx: 27
- sleep-lior-2-6794d4cfdc-mp8sn: 29
- sleep-lior-2-6794d4cfdc-p594m: 27
- sleep-lior-2-6794d4cfdc-qnlnl: 27
- sleep-lior-2-6794d4cfdc-rvmd2: 25
- sleep-lior-2-6794d4cfdc-stjzd: 26
- sleep-lior-2-6794d4cfdc-tffd9: 27

Performance Metrics:
- Total CPU Usage: 10 CPUs
- CPU Usage Range: 2 (highest pod) to 0.2 (lowest pod)

Latency Metrics:
- p50 Latency: 14ms (ranging from 50ms to 6ms)
- p90 Latency: 38ms (ranging from 100ms to 10ms)
- p95 Latency: 47ms (ranging from 170ms to 17ms)
- p99 Latency: 93ms (ranging from 234ms to 23ms)
- Request Rate per Pod: 100 requests per second (rp/s)

### Results After Optimization

Performance Improvements:
- Total CPU Usage: 8 CPUs
- CPU Usage Range: 0.6 (highest pod) to 0.45 (lowest pod)

Latency Metrics:
- p50 Latency: 13.2ms (ranging from 23ms to 9ms)
- p90 Latency: 24ms (ranging from 46ms to 21ms)
- p95 Latency: 33ms (ranging from 50ms to 23ms)
- p99 Latency: 47ms (ranging from 92ms to 24ms)
- Request Rate per Pod: Adjusted, with the fastest pod handling 224 rp/s and the slowest pod handling 25 rp/s

### Interpretation of Results

The optimization demonstrated significant performance improvements:

CPU Usage Reduction:
- The total CPU usage of all the pods decreased from 10 CPUs to 8 CPUs, indicating more efficient resource utilization.

Latency Reductions:
- p50 Latency: Decreased from 14ms to 13.2ms
- p90 Latency: Improved drastically from 38ms to 24ms
- p95 Latency: Went down from 47ms to 33ms
- p99 Latency: Nearly halved from 93ms to 47ms

Balanced Load Distribution:
- Post-optimization, request rates adjusted dynamically to ensure that faster pods handle more requests (up to 224 rp/s), and slower pods handle fewer requests (down to 25 rp/s), contributing to lower latencies and balanced resource usage.

## Conclusion

By focusing on CPU metrics and dynamically adjusting load balancing weights, we optimized the performance of our microservices running in a heterogeneous hardware environment. This approach, facilitated by a custom Kubernetes operator and leveraging Istio and Envoy, enabled us to reduce tail latency and improve overall system reliability significantly.

Maintaining high performance in distributed systems can be challenging, but with intelligent automation and a focus on critical metrics, significant improvements in consistency and reliability can be achieved. Our experience demonstrates that adapting load-balancing strategies to account for hardware variability can overcome performance disparities and create a more responsive and robust microservices architecture.

## Google Search

Extensive Google searches led us to an article detailing Google's innovative methods for similar issues. This discovery was transformative, affirming that load balancing of least connections is a common challenge. Google developed an internal mechanism called Prequal, which optimizes load balancing by minimizing real-time latency and requests-in-flight (RIF), a concept not found in Envoy's load balancing.

## Engaging with the Community

Rather than rushing into developing the Kubernetes controller, we first engaged with the community. We inquired whether anyone knew of a built-in mechanism or an existing open-source add-on that could solve our problem. This approach, while time-consuming, was crucial for our effectiveness. It not only saved us time but also provided us with valuable insights. For example, during our tests, we encountered a bug that the community resolved in less than 24 hours, demonstrating the power of collaborative problem-solving.

Example Community Interaction:
We raised an issue on Istio's GitHub repository <https://github.com/istio/istio/issues/50968> and witnessed a swift response from the community, highlighting the importance of collaboration.

## Plans

Our proof of concept (POC) is currently running in alpha mode in production and performing well. We've quickly implemented the first step of balancing only CPU resources, and it's effective so far. We've decided to give the Envoy and Istio communities time to integrate something similar to Google's RIF solution. Our next steps include:

1. Monitoring and Iteration: Continuously monitoring the performance and making necessary adjustments.
2. Exploring Additional Metrics: Considering other metrics such as memory usage or network latency for finer load balancing.
3. Community Collaboration: Working with the Istio and Envoy communities to contribute our findings and improvements back to the open-source projects.

Through targeted optimization and community collaboration, we believe our approach can serve as a blueprint for others facing similar challenges in heterogeneous Kubernetes environments.
