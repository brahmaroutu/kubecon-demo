#!/bin/bash

source ./common.sh

DIR=$(pwd)
cd artifacts/deploy
comment "Let us create a namespace demo"
doit more namespace.yaml
#doit kubectl create -f namespace.yaml
comment "We also need to have permission to create object"
doit more rbac.yaml
#doit kubectl create -f rbac.yaml

comment "We are ready to now deploy our custom controller that handles our Foo type objects in k8s"
#doit kubectl create -f kubecon-demo.yaml -n demo
comment "Check to see if two pods are running. One of these pods is active and the second one is passive controllers."
doit kubectl get pods -n demo
nodes=(`kubectl get pods -n demo -o jsonpath="{range .items[*]}{.metadata.name}{\"\t\"}{.spec.nodeName}{\"\n\"}{end}" | awk '{print $1}'`)
for i in "${nodes[@]}"
do 
  doit kubectl logs --tail=100 $i -n demo 
done

comment "Let us see how your resource is synced properly."
doit kubectl describe foo example-foo

comment "To update the Foo object so that the controlleris actively working on the changes let us deploy a test application"
comment "The test application is basically changing the number of instances, this way the controller is constantly doing some work reconciling the changes."
doit kubectl run -n demo --image="hweicdl/poker:v0.1.0" poker
