# go-pipeline-demo
**Go plus GCP demo project**

I wrote this over a weekend as a proof of concept to execute a golang pipeline with channel-
and database-driven functionality.  I also wanted to host the app on Google Cloud.

Big Note: I used Claude a ton to do this.  I know a bit about go, but am far from an expert
at it.  And while I have some AWS in my toolbox I have no CGP experience.  So I built a
project specification and asked Claude to help me get the project written and hosted on
google cloud within a weekend timeframe.

My intent here is to create somethng that works, and then use it as a basis for learning
what are the parts are (it's easier to google things when you know what the terms are).

The app is a pipeline that generates 5-digit numbers and uses a combination of firestore-
stored state and channels to perform a few operations on those numbers.  The code is stored
in this repo and is used to build an instance on CGP.

I'm trying to get a cloud build trigger based on changes to main, but I'm having trouble
with quotas and regions, so I have to settle for manual deployments.  Once I get the
trigger issue sorted out I will chase down some bugs that exist in the application
(sometimes it tries to close an already closed channel, and I'm not convinced that the
FSM really works.  Then I'll expand my understanding of GCP and k8.

Endpoints:
- curl -X POST http://<HOSTNAME>/run
- curl http://<HOSTNAME>/stats

**Claude Prompt:**
I am a software engineering manager, with a background in AWS and a few different languages.
I need to learn google cloud, kubernetes, and get better at golang.

I'd like to learn as much as I can in the next 2 days, with a goal of creating an application
that I can put into a github repo and also host on google cloud.

Some basic notes:
- Something that works is better than something that is sophisticated.  I'm not trying to trick
anyone into thinking that I am an expert.  I'd rather be able to explain what I've done with a
basic setup than create something that's sophisticated but that I can't explain.  I need to
understand the things that you help me with.

- Something practical is better than something theoretical.

- I have a basic understanding of go structures, channels, and goroutines, but I haven't coded a
ton.  I think I'd like to create a pipeline that performs a series of processing steps.  I'd
like to have a fairly minimal struct for each pipeline step, and to store ancillary and state
information in a database.  This might be overkill for the simplicity of the task, but I'd like
to demonstrate the ability to use a database to house some information in order to keep the payloads
(and thus the channel) small.

- I don’t have a lot of kubernetes experience.  I’ve made use of it when deploying projects, and I
know that it's used for orchestration.  I imagine that I will want to create an image (maybe a small
generic OS with my repo added to it?) and then deploy it.

- This is intended as a demo.  I want to show someone that I can go from almost no knowledge to a
full solution in a short time, but if we build something that I don't understand, then I won't be
able to explain it well.
- I would like to store a small amount of information in the struct used in the channel, and provide
more information in a NoSQL database.  I’d like to use the same ID for the channel and the database.

Here's my proposed project:
- Configuration: I’d like to store the following:
* verbose: show information about each item as it moves through the pipeline.  Values: true or false
* total items: how many items to run through the pipeline.  Value: integer
* Dispatch size: the number of items with a state of “produced” to read from the database during the dispatch step of the pipeline.  Value: integer.
* General channel size: the size of the processing channels.  Value: integer
* Database channel size: the size of the channel used by the transition step when communicating with the database
* Force panic probability: whether to create an error condition when the transition step fires.  Value: floating point between 0 and 1, which indicates how likely a given execution is to throw an artificial error.

Item Struct (for the channel):
- ID (UUID)
- Value (semantically an integer, but stored as a string of length 6)

Struct for state transitions (for history, to be stored in the database).  I expect to report this information, but don’t need to query it directly, so I don’t need any indexing on it.  The fields are:
- State
- Value
- Timestamp when created

Struct for an item (for the database representation of an item).  This is a larger structure, and is used not only to track state but also to trigger some transitions.  It will be used for reporting.  The fields are:
- ID
- Current state
- Current value
- State history (a slice using the state transition struct defined above)
- Timestamp for creation
- Timestamp for update

Pipeline: the processing pipeline has the following steps:
- “Transition”.  This accepts an ID, a value, and a state.  It then either creates or updates a record in the database.  It should trigger a panic with a probability specified in the configuration.  This panic should be triggered before processing.
- "Produce".  This step produces a number (specified in the configuration) of item structs: a random 5-digit number (to be stored as a string)  and a UUID as an ID.  After all items have been created, they should be sent to the “transition” step, with a state of “produced”.
- “Dispatch”.  This step looks in the database for items with a state of “produced”.  It selects all such items and in batches whose size is number of items set in the configuration.  It performs the following steps for each of them:
* Invoke the transition step of the pipeline, with a state of “dispatched”.
* Send the item to “check representation" via channel.
- “Check representation”.  If the value has the same digit in 3 places, invoke the transition step, with a state of “representation problem”.  Otherwise send the new value to “Get even” via channel.
- "Get even".  Remove all odd digits from the value.  If the value is now less than 2 digits long, invoke the transition step, with a state of “size problem”.  Otherwise send the item to “triple” via channel.
- “Triple”.  If the value multiplied by 3 is 5 digits long, divide the value by 2 and re-send the item to “triple” (with the new value) via channel.  Otherwise multiply the value by 3 and send the new item to the “transition step” with a state of “tripled”.
- "Reverse".  This step looks in the database for a number of items specified in the configuration with a state of “tripled”.  For each item it will:
* Reverse the digits of the value.
* Invoke the transition step with a state of “reversed”.

Panic handling:
If a panic is triggered, the item should be sent to the transition step, with a state of “induced error”.

I need an endpoint to run the following steps:
- Produce
- Dispatch
- reverse

I'd also like to have 2 reporting processes:
- Given an item ID (UUID) show the various pvalues that the item has hadas its gone from state to state.
- For a specified datetime range, give a count of the items that have been produced, that have made it to the final state, and that have ended up in the various error states.

Misc notes:
- I'd like this pipeline to operate as a finite state machine.
- I don't have strong opinions about (or knowledge of) the database that stores the state, but I imagine that a noSQL solution would be best.
- For the pipeline trigger, let's start with an endpoint to trigger.  I hope that we can add a cron to it later. 

